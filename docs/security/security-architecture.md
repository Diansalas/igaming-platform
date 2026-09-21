# Security Architecture Proposal

Status: Stage 0 draft. Owned by the `security` specialist going forward.
Source: Blueprint §4.1, §4.6, §4.7, §4.8, §7, plus `CLAUDE.md`.

## Authentication and session model

- Brand-frontend players: short-lived JWT per session, tenant-scoped.
- Game/product launch: separate single-use opaque token bound to
  `(player, provider, product, currency, mode)`, short TTL — never the
  session JWT. A leak in one of many provider integrations must not become
  account takeover platform-wide.
- Back office / partner console staff: separate authentication path from
  players, with RBAC scoped by tenant.

## Authorization

Three-tier RBAC (platform admin / partner admin / brand operator).
Enforced server-side on every request; never inferred from UI state;
`tenant_id` always derived from authenticated context, never accepted from
the client.

## Tenant isolation

PostgreSQL row-level security bound to connection-level tenant context is
the enforcement mechanism (see `docs/architecture/03-database-
architecture.md`). Every new endpoint requires a test proving a valid
token for tenant A cannot read or write tenant B's data.

## Secrets

Vault or a cloud KMS. Provider HMAC keys rotate; PSP credentials are
per-tenant. No secret ever lives in a config file committed to git, an
environment variable dumped to logs, or an API response.

## PCI scope

Hosted fields/redirect only for card data. No PAN ever reaches platform
infrastructure.

## Audit

Every mutating action (financial, administrative, compliance-relevant)
writes an immutable audit record: actor, tenant, entity, before/after
state, IP, reason code. 5–7 year retention, exportable. Manual balance
adjustments require reason code + four-eyes approval above a configurable
threshold.

## Threat model priorities for Stage 0/1

1. Cross-tenant data access (highest architectural risk given the
   multi-tenant model).
2. Session/token leakage into a third-party provider integration.
3. Idempotency-key forgery or replay on financial endpoints (owned jointly
   with `ledger-finance`).
4. Privilege escalation in back-office/partner-console RBAC.
5. Secrets exposure via logs, error messages, or client-visible responses.

## What this document does not cover

Formal penetration testing, GLI-19/ISO 27001 certification audits, and
legal/regulatory security requirements specific to a jurisdiction — those
require external, human-run engagements and are tracked as dependencies in
`docs/architecture/13-dependency-map-and-risk-register.md`, not
substituted for by this document.

## Review cadence

`security` reviews every change touching auth, sessions, tokens, RBAC,
secrets, or PII handling before it is marked `IMPLEMENTED`, per
`.claude/agents/security.md`.

## Stage 4H-B0-R5 — independent security review of five architecture P1s

`security`-owned record of findings raised against ADR 0037 (§B.6/§B.7,
Part C), ADR 0038 (§14/§14.6), `ledger-accounting-model.md` §6.3, and ADR
0034 §14. Architecture-only review; no code exists for any of these
subsystems. Each finding below is routed to the specialist who owns the
file — `security` does not edit another specialist's architecture
document (CLAUDE.md, "no specialist redesigns shared architecture
unilaterally").

**S-1 (P1, ADR 0037 Part B/C, owner `architect`).** No administrative
operation in ADR 0037 §C.5.1 covers the FX control plane: the per-pair
`FXRateProvider` priority list (§B.3), `max_age` (§B.6 item 2), the
magnitude-deviation bound (§B.7.2), and the cross-provider spread bound
(§B.7.3). Those values are the entire quantitative defense of the
conversion path, and their RBAC tier, dual-control requirement, and audit
obligation are undefined. "Absent configuration fails closed" does not
protect against a *present but deliberately widened* bound.

**S-2 (P1, ADR 0037 §B.7.2, owner `architect`/`ledger-finance`).** The
magnitude-plausibility baseline is the *same provider's own*
`GetHistoricalRate`. A compromised provider controls both sides of that
comparison. The platform's own persisted `applied` Conversion records
(§B.4) are an independent, platform-controlled baseline and should be the
primary anchor, with the provider's history at most corroborating.

**S-3 (P1, ADR 0037 Part C, owner `architect`).** `assets` has no RLS and
no `tenant_id` (migration `0003`, by design — it is platform-wide), so the
two-tier split's protection for layers 1-3 is an application permission
check only, with no database backstop. Layers 4-6 are mechanically
enforced (`tenant_jurisdiction_configs` RLS); layers 1-3 are not.
A platform-scoped write path needs its own DB-level guard.

**S-4 (P1, ADR 0037 §C.5.5, owner `architect`).** `assets.active` is
`BOOLEAN NOT NULL DEFAULT true` in migration `0003`. §C.5.5's "creation
forces `active = false`" is an API-level rule contradicted by the live
schema default; any insert path that omits the column fails open. The
implementing migration must flip the default and add
`platform_authorized NOT NULL DEFAULT false`.

**S-5 (P1, ADR 0038 §14.1, owner `ledger-finance`/`sportsbook`).** The
DB uniqueness key becomes an *adapter-composed* string; in the fallback
branch its distinguishing component derives from the adapter's own
delivery observation rather than from a provider-signed field. This
breaks the property the casino callback precedent relies on (the
uniqueness key is a value the provider signed, so verbatim replay
collides). Replay of a validly signed body as a new delivery yields a new
ordinal, a new composed key, no constraint violation, and a second
posting.

**S-6 (P1, ADR 0038 §14.1/§14.6, owner `ledger-finance`).** The
composition `{provider reference}#{ordinal}` concatenates an opaque,
externally-supplied `TEXT` field into a uniqueness key with no delimiter
reservation, escaping rule, or charset validation. Two distinct events
can be made to compose to the same key, silently absorbing the second as
a duplicate (a missed post).

**S-7 (P1, `ledger-accounting-model.md` §6.3.3.1, owner
`ledger-finance`).** The remaining-per-origin recovery query is a
balance-sufficiency read feeding a debit decision, so invariant #15
applies: it must execute in the same database transaction as the posting
it authorizes, under the same `(tenant_id, provider_id, provider_tx_id)`
advisory lock Stage 4G-FINAL Part F added for concurrent deliveries.
Neither is stated, and an empty/short result has no specified
fail-closed handling.

**S-8 (P1, ADR 0034 §14.3/§14.8, owner `identity-compliance`).** The
policy version is described both as "the version in effect when
self-exclusion becomes effective" and as "resolved fresh... never
cached." The applying listener runs after the commit, so `now()`-based
resolution lets a configuration change inside that window govern. The
as-of anchor must be the self-exclusion's own effective timestamp.

**S-9 (P1, ADR 0034 §14.5, owner `identity-compliance`).** One audit
record per affected bet cannot evidence *completeness* of the
enumeration. A dropped `rg.status.changed` event leaves no record that
any bet was skipped, which is precisely what §14.5 claims the record
proves. A per-self-exclusion-event enumeration-completion record plus a
reconciliation sweep is required.

**Authorization / tenant-isolation tests every affected domain's `qa`
coverage must include** (`security`-specified, per
`.claude/agents/security.md`):

- A request for tenant A's asset/eligibility/conversion data using
  tenant B's valid token returns 403/404, never data.
- A tenant-scoped staff token attempting every ADR 0037 §C.5.1 layer-1-3
  operation is denied, including when the tenant-scoped permission for
  layers 4-7 is held.
- A tenant-scoped override that would *widen* past a platform-layer
  denial is rejected at write time and cannot take effect at resolution
  time.
- A four-eyes-required asset operation with one approver, and with the
  same approver twice, both fail — mirroring
  `withdrawal_approvals`' `UNIQUE (request_id, approver_principal_id)`
  and its self-approval trigger.
- `CheckEligibility` with a zero-value jurisdiction denies (a zero brand
  is permitted; a zero tenant or jurisdiction is not).
- A verbatim replay of a signed sportsbook settlement callback delivered
  twice as two separate transport deliveries posts exactly once.
- A crafted provider reference containing the composition delimiter
  cannot collide with another occurrence's key.
- A tenant/brand-scoped `OpenBetSelfExclusionPolicy` row that loosens a
  jurisdiction floor never governs any bet's disposition.

---

## Stage 4H-B1 Wave 1 — Bonus Engine security / RBAC / audit / tenancy contract

Status: **DESIGN / CONTRACT ONLY — `NOT IMPLEMENTED`.** No code, no
migration, and no permission constant is authorized by this section. It
is the contract the Bonus Engine's later waves build *against*, owned by
`security`, produced for Stage 4H-B1 Wave 1.

Scope of this section: RBAC permission set and role wiring, four-eyes
enforcement pattern, RLS/tenancy shape for the new bonus tables, and the
audit event catalogue. **Out of scope, stated rather than implied**: the
bonus domain model itself (entity fields, state-machine internals,
ledger postings), the API route table, and any judgement that the Bonus
Engine is "secure" — nothing has been built to review. Table names below
are *placeholders* pending `bonus-engine`'s Wave-1 domain model (§B1.7).

### B1.0 Standing rule for every migration in this stage (migration 0048 precedent)

**No migration in Stage 4H-B1, from any specialist, may use a
transaction-local `ALTER TABLE ... NO FORCE ROW LEVEL SECURITY` /
`... FORCE ROW LEVEL SECURITY` toggle — for any reason, including a
diagnostic row count in a guard's error message.** This was finding
**S-1** against migration `0048`'s first draft and it is blocking, not
advisory. The failure scenario is concrete: the `FORCE` restore is
transaction-local, so an operator running the file standalone under
`psql -v ON_ERROR_STOP=1 -f` — precisely what they would do to read a
guard's message during an incident — stops at the `RAISE` and leaves the
table with `FORCE` permanently off. That is a silent, permanent loss of
tenant isolation on a table, introduced by the very statement meant to
make a control legible.

Two corollaries that apply directly to bonus migrations:

1. **A migration-time `SELECT` against a `FORCE ROW LEVEL SECURITY`
   table is silently inert.** A migration connection sets no
   `app.tenant_id`, so it reads zero rows regardless of contents. Every
   bonus table specified below carries `FORCE`, so any pre-flight guard
   in a bonus migration must use a mechanism RLS cannot filter —
   `ADD CONSTRAINT ... EXCEPTION WHEN check_violation` (migration
   `0048`'s final shape) or an equivalent — never a row count.
2. `TRUNCATE` is not subject to RLS at all. Every bonus table needs its
   own `BEFORE TRUNCATE ... FOR EACH STATEMENT` deny trigger (migration
   `0047` Fix 3's closing point).

### B1.1 RBAC permissions

Naming follows the live convention in `internal/auth/permission.go`:
`<resource>:<verb>` for an action authority, `<resource>_config:<verb>`
for a configuration authority. `docs/architecture/25-bonus-gamification-
api-architecture.md` §2 already proposed `bonus:read`, `bonus_config:read`
and `bonus_config:manage`. `bonus:read` and `bonus_config:read` are
carried forward **verbatim**. `bonus_config:manage` is **split** into six
narrower authorities below, because as a single permission it bundles
"author an Offer that mints value automatically" with "define who
receives it" with "turn it on" — the exact bundling that document's own
F2/F3 findings identified and that the platform has already un-bundled
twice (`withdrawal:*` four ways, `risk_config:read`/`:manage`).
**Flagged for `architect` / `backend-platform`**: this refines a
permission name defined in a document `security` does not own; it is a
proposal routed to its owner, not a unilateral redesign (CLAUDE.md,
"no specialist redesigns shared architecture unilaterally").

| Permission | Gates | Four-eyes above tier? |
|---|---|---|
| `bonus:read` | Staff read of one player's Grant / Progress / Conversion / Adjustment history | n/a |
| `bonus_config:read` | Read Campaign, Offer (all versions), Segment, BonusCode, BulkJob definitions and bonus engine configuration | n/a |
| `bonus_campaign:create` | Create a Campaign. Always created inactive — creation never implies activation | No |
| `bonus_campaign:update` | Edit an inactive Campaign's metadata/scope; bind a new Offer version to it | No |
| `bonus_campaign:activate` | inactive → active **only** (the off→on direction) | **Yes** |
| `bonus_campaign:suspend` | active → inactive/paused **only** (on→off, incident kill-switch) | No — deliberate asymmetry |
| `bonus_offer:manage` | Author and publish an Offer version's rule content (the five axes) | **Yes**, on publication, above reward-value tier |
| `bonus_segment:manage` | Define/edit Segments; issue and revoke BonusCodes | **Yes**, on BonusCode issuance above aggregate-value tier |
| `bonus_grant:issue` | Staff-initiated single Grant issuance and manual activation | **Yes**, above per-grant value threshold |
| `bonus_grant:review` | Work the abuse/manual-review queue: claim, annotate, record a finding. Confers **no** power to change a Grant's state | No |
| `bonus_grant:cancel` | Staff-driven cancellation / forfeiture of an existing Grant | **Yes**, only when the Grant is in `completed` (see §B1.2 item 7) |
| `bonus_adjustment:write` | Direct bonus-balance adjustment; staff-forced conversion / manual release override; manual override of a computed reward amount | **Yes**, threshold defaults to 0 (= always) |
| `bonus_bulk:execute` | Submit and execute a BulkJob (mass grant / mass adjustment) | **Yes**, always — see §B1.2 item 2 |
| `bonus_report:read` | Aggregate campaign performance / cost / outstanding-liability reporting. Deliberately **not** per-player: holding it must never disclose an individual player's identity, PII, or KYC-derived state | n/a |
| `bonus_approval_policy:write` | Write the `bonus_approval_policies` rows that set the four-eyes thresholds themselves | No (append-only, always audited) |

**Audit — deliberately no new permission.** `audit:read` already exists,
`audit_log` already has dual-scope RLS (ADR 0013), and every bonus audit
record lands in that one table. Minting `bonus_audit:read` would fragment
the audit surface into per-domain permissions and create a role that can
read bonus audit records but not the withdrawal/adjustment records
describing the same money — which is worse for investigation, not better.
Bonus-domain *forensic* detail lives in the Progress trail, read via
`bonus:read`. Recorded as a decision the orchestrator may override, not
an omission.

#### Role wiring

One new role is required: **`RolePromotionsManager`** (`promotions_manager`
— the name `25-bonus-gamification-api-architecture.md` §2 already
reserved as a build-time requirement). A second, **`RoleBonusOperations`**
(`bonus_operations`), is required to hold the financially material set,
for the reason given immediately below.

| Role | Bonus permissions |
|---|---|
| `RolePromotionsManager` (new) | `bonus_config:read`, `bonus:read`, `bonus_report:read`, `bonus_campaign:create`, `bonus_campaign:update`, `bonus_campaign:activate`, `bonus_campaign:suspend`, `bonus_offer:manage`, `bonus_segment:manage` |
| `RoleBonusOperations` (new) | `bonus_config:read` (read-only, to see what a Grant was issued under), `bonus:read`, `bonus_grant:issue`, `bonus_grant:review`, `bonus_grant:cancel`, `bonus_adjustment:write`, `bonus_bulk:execute` |
| `RoleTenantAdmin` | `bonus_config:read`, `bonus:read`, `bonus_report:read`, `bonus_approval_policy:write` — **and nothing else** |
| `RoleCompliance` | `bonus:read`, `bonus_config:read`, `bonus_campaign:suspend` |
| `RoleRiskManager` | `bonus_config:read`, `bonus_campaign:suspend` |
| `RoleSupport` | `bonus:read` |
| `RoleFinance` | **none** |
| `RolePlatformAdmin` | **none** |

Each exclusion is a decision, not an oversight:

- **`RoleTenantAdmin` gets read-only, exactly as it does for
  `risk_config`, `verification` and `rg_restriction` today.** This is the
  direct answer to "do not let a tenant admin self-escalate into finance
  powers". A tenant admin may see every campaign and every player's grant
  history, and may not author, activate, issue, adjust, bulk-assign or
  cancel anything.
- **`RoleFinance` gets nothing.** Granting `bonus_adjustment:write` to
  the role that also approves withdrawals creates a clean two-step drain:
  credit a colluding player's bonus balance below the adjustment
  threshold, then approve the resulting withdrawal below the withdrawal
  threshold. Neither step needs a second human. `RoleFinance`'s existing
  "withdrawal governance and nothing else" shape is preserved.
- **`RolePlatformAdmin` gets nothing**, for the reason every other
  platform-admin exclusion in `permission.go` already gives: a
  platform-scoped principal cannot resolve a tenant's `player_account` at
  all, so the permission would be a capability nothing can use. See
  §B1.3's "no platform-wide Campaign in Wave 1" recommendation, which is
  what makes this consistent rather than a gap.
- **`bonus_campaign:suspend` is granted widely** (promotions, risk,
  compliance) precisely because it is the fail-closed direction. Migration
  `0044`'s own asymmetry rationale applies verbatim: "an emergency
  kill-switch must not need a second approver" — and it must not need a
  scarce role-holder either.
- **`bonus_grant:review` confers no state-change power.** The reviewer
  recommends; `bonus_grant:cancel` executes. Both live in
  `RoleBonusOperations` today, so this is currently a logging/ordering
  separation rather than a hard one — recorded honestly as such, and it
  becomes a hard separation the moment a tenant staffs the two roles
  separately.

**Hard constraints on wiring, to be enforced as code and tested:**

1. No principal may hold both `RolePromotionsManager` and
   `RoleBonusOperations`. The author of a Campaign must not also be able
   to hand out its value directly. (Enforced at staff-account creation;
   there is no multi-role model in `internal/auth` today — one role per
   staff account — so this reduces to "two accounts, two humans", which
   the four-eyes person check in §B1.2 independently verifies.)
2. Neither new role may ever hold `staff:manage`. Stage 3D's business
   decision #4/#5 precedent, verbatim.
3. No role may hold both `bonus_offer:manage` and
   `bonus_adjustment:write` — the "configure it instead of adjusting it"
   bypass (doc 25's F3) is only closed if authoring and adjusting are
   separate authorities *and* both are threshold-gated.
4. No role may hold both `bonus_approval_policy:write` and any
   four-eyes-gated bonus permission. This is
   `PermWithdrawalPolicyWrite`'s own stated rationale applied unchanged:
   "the role that approves must not also be the role that can loosen the
   policy gating its own approvals." It is why
   `bonus_approval_policy:write` sits on `RoleTenantAdmin` (which holds
   no bonus write authority) rather than on either new role.

#### **P1 — the staff-creation allowlist must be extended, or constraint 1 and 2 are both defeated on day one**

`internal/httpserver/admin_routes.go` restricts *which roles a caller may
create*. Today (line ~262) the allowlist is
`RequireOneOf("role", req.Role, "tenant_admin", "support", "compliance",
"finance", "risk_manager")`, and `finance` (~line 277) and `risk_manager`
(~line 287) additionally require a **platform-scoped** caller.

`RoleTenantAdmin` holds `staff:manage`. Concrete failure scenario if the
new roles are added to the allowlist without the platform-scoped
restriction: a tenant admin — who by design holds no bonus write
authority — creates a `bonus_operations` staff account, choosing its
password and `person_id`, logs in as it, and holds
`bonus_adjustment:write` and `bonus_bulk:execute`. Removing the
permission from `RoleTenantAdmin`'s permission set does nothing, because
the new account's permissions come from *its own* role. This is the exact
escalation path Stage 3C's review found for `finance` and Stage 3D closed
in the handler, not in the permission map. Both new roles **must** be
added to the allowlist *and* to the platform-scoped-caller restriction,
in the same change that mints them. Wave 1 records this as a mandatory
implementation requirement; it is a blocking finding if the roles land
without it.

Secondary consequence, flagged for `identity-compliance`: if both bonus
roles are platform-creatable only, a tenant cannot self-serve its own
promotions staffing. That is the same operational cost `finance` and
`risk_manager` already pay, and it is the correct trade — but it is a
real cost, not a free win.

### B1.2 Four-eyes requirements

Operations requiring dual control, each with the trigger that makes it
economically material:

1. **Manual Grant issuance** (`bonus_grant:issue`) at or above a
   configurable per-grant value threshold, denominated in the reward
   asset's minor units.
2. **BulkJob execution** (`bonus_bulk:execute`) — **always**, regardless
   of per-player value. A bulk job's blast radius is
   `recipients × value`, and a job that is individually trivial per
   player is not trivial in aggregate. A per-player threshold applied to
   a bulk job is not a control; it is an accounting error waiting to be
   discovered by reconciliation. Additionally: the approved request must
   pin the recipient set (a content hash of the resolved recipient list
   plus its row count) so the set cannot be swapped between approval and
   execution.
3. **Direct bonus Adjustment** (`bonus_adjustment:write`) at or above a
   configurable threshold whose **default is 0** — i.e. every adjustment
   requires four-eyes unless a tenant explicitly configures otherwise.
   This mirrors `internal/withdrawal/policy.go`'s fail-closed fallback
   exactly (`ThresholdAmount: 0, RequiredApprovals: 2` when no policy row
   resolves) and CLAUDE.md's own "manual balance adjustments require a
   reason code and four-eyes approval above a configurable threshold".
4. **Staff-forced conversion / manual release override** — same
   permission and same threshold as item 3; it is an adjustment that
   happens to be expressed as a state transition.
5. **Campaign activation** (`bonus_campaign:activate`) where the
   Campaign's cost tier exceeds a configurable bound. Cost tier must be
   computed server-side from the bound Offer version's maximum
   per-player reward value and the Campaign's own budget cap — never
   accepted from the client, and never inferred from a field the author
   also controls without it being part of the approved payload.
6. **Offer version publication** (`bonus_offer:manage`) where the Offer's
   maximum per-player reward value exceeds the item-1 threshold. This is
   doc 25's F3 finding made mechanical: without it, an actor who cannot
   issue a 10,000-unit manual grant can author a narrow-eligibility Offer
   that grants 10,000 units automatically, and the four-eyes control is
   theatre.
7. **Cancellation/forfeiture of a Grant already in `completed`**
   (`bonus_grant:cancel`) above a configurable value threshold.
   **New requirement, not inherited from an existing precedent, with its
   reasoning stated**: a `completed` Grant is a fully wagered-through,
   *earned* entitlement; cancelling it posts `Dr player_bonus / Cr
   promo_liability` (ADR 0032 §5) and extinguishes it. That is
   confiscation of an earned balance by a single actor, and per
   `10-bonus-engine-architecture.md` §10 it is "the exact record a
   disputing player's case turns on". Cancellation of a Grant in any
   pre-`completed` state stays single-actor — nothing earned has been
   taken, and an abuse response must not wait for a second approver.

**Deliberately NOT four-eyes** (the fail-closed direction, matching ADR
0037 §C.5.3's asymmetry): campaign suspension/deactivation, Offer
deprecation, BonusCode revocation, Segment narrowing, and cancellation of
a pre-`completed` Grant. Turning something off must never require a
second approver.

#### Enforcement pattern — trigger-based, mirroring 0044/0047 with 0047's AFTER-trigger correction

Two new tables, tenant-scoped (unlike `asset_change_requests`, which is
platform-scoped because the assets it governs are):

- **`bonus_change_requests`** — `id`, `tenant_id NOT NULL`, `brand_id`,
  `operation` (CHECK-constrained to exactly the seven dual-controlled
  operations above; a suspend/revoke can never be filed, so it can never
  be made to look like it needed an approval it didn't),
  `target_type`, `target_id`, `payload JSONB NOT NULL`,
  `amount_at_request`, `asset_code`, `reason_code NOT NULL`,
  `requested_by_principal_id`, `requested_at`, `state`
  (`pending`/`applied`/`rejected`/`cancelled`),
  `applied_by_principal_id`, `applied_at`.
- **`bonus_change_approvals`** — `id`, `tenant_id NOT NULL`,
  `request_id`, `approver_principal_id`, `decision`
  (`approve`/`reject`), `reason_code` (required on reject),
  `decided_at`, **`threshold_at_decision`** and
  **`amount_at_decision`**, `UNIQUE (request_id, approver_principal_id)`.

The two `*_at_decision` columns are `withdrawal_approvals`'
`threshold_amount_at_decision` precedent, and they are load-bearing: they
are what stops a later threshold change from retroactively making a
past decision look compliant (or non-compliant) when the record is read
during a dispute.

Enforcement stack, in order:

1. `UNIQUE (request_id, approver_principal_id)` — one principal can never
   count as two approvals (migration `0026`).
2. **Immutability.** `BEFORE UPDATE` on `bonus_change_requests` rejecting
   any change to `operation`/`target_*`/`payload`/`amount_at_request`/
   `reason_code`/requester/`requested_at`, and rejecting any transition
   out of a non-`pending` state; `BEFORE DELETE` and
   `BEFORE TRUNCATE` deny. `ledger_deny_mutation()` on
   `bonus_change_approvals` for `UPDATE OR DELETE` plus a truncate guard.
   Without this, the control is bypassed by filing a harmless request,
   collecting the approval, then rewriting the payload.
3. **Governance trigger on approval insert — modelled on migration
   `0034`'s `withdrawal_approvals_enforce_governance()`, explicitly NOT
   on migration `0029`.** This is `0047` Fix 1's lesson: `0044` mirrored
   `0029`, whose person comparison was conditional on both `person_id`s
   being non-NULL, and it turned out to be unconditionally inert. The
   bonus trigger must therefore, for **both** requester and approver:
   refuse if the principal cannot be resolved to a `staff_users` row;
   refuse if `status <> 'active'`; refuse if `person_id IS NULL`; refuse
   if `tenant_id` does not equal the request's `tenant_id` (the
   tenant-scoped analogue of `0044`'s platform-scope check); and then
   compare `person_id` **unconditionally** — two staff accounts held by
   one human are one human. No service-identity carve-out: unlike a
   below-threshold withdrawal auto-approval, no service identity files or
   decides a bonus adjustment.
   *Deploy-ordering note (`0047`'s own warning, applied):* tenant-scoped
   staff **can** be person-linked today, so unlike `0047` this does not
   brick anything — but the same pre-deploy verification must be run per
   tenant: `SELECT id, email FROM staff_users WHERE tenant_id = <t> AND
   status = 'active' AND person_id IS NULL` must return zero rows, or
   that tenant's promotions staff cannot file or approve anything.
4. **A single consume function**,
   `bonus_change_consume_approved_request(p_tenant_id, p_operation,
   p_target_id, p_payload_match JSONB)`, mirroring
   `asset_change_consume_approved_request`'s 3-argument form: select the
   oldest `pending` request matching `(tenant, operation, target)` whose
   `payload @> p_payload_match`, `FOR UPDATE`, requiring at least
   `required_approvals` distinct `approve` rows all with
   `approver_principal_id <> requested_by_principal_id`, and **no**
   `reject` row at all (one approver's "no" is not overridable by a third
   approver's "yes"); then mark it `applied` **in the same statement**.
   Consuming inside the trigger, not in Go afterwards, is what makes one
   approval authorize one mutation, once.
   The JSONB payload match is not optional decoration: for a bulk job it
   pins the recipient-set hash, for an adjustment the amount and asset,
   for an activation the Offer version id. Approving "some adjustment for
   this player" is not dual control.
5. **The enforcement trigger is `AFTER INSERT OR UPDATE ... FOR EACH ROW`,
   never `BEFORE`.** This is migration `0047`'s explicit correction to
   `0044`, and it applies to the Bonus Engine *more* strongly than it did
   to asset eligibility, because `10-bonus-engine-architecture.md` §9
   specifies every bonus write as upsert-shaped
   (`(tenant_id, campaign_id, offer_version_id, player_account_id,
   trigger_reference)` and friends). PostgreSQL fires `BEFORE INSERT`
   first, *then* detects the conflict and fires `BEFORE UPDATE` — so one
   `INSERT ... ON CONFLICT DO UPDATE` fires a BEFORE trigger twice, and
   the first firing's side effects are not undone. A consume in a BEFORE
   trigger would either demand two approvals for one logical operation or
   consume one and then fail. An AFTER row trigger fires exactly once for
   the row that survives the statement, with the true `OLD` for the
   conflict case. A `RAISE` in an AFTER trigger still aborts the
   statement, so the control is no weaker for being AFTER.
   **Standing rule for this stage: no bonus migration may implement a
   dual-control consume in a BEFORE trigger.** Pure, side-effect-free
   validation may stay in a BEFORE trigger (that is what `0047` left
   `asset_operation_eligibility_enforce_narrowing()` alone for).
6. **Threshold resolution** comes from a `bonus_approval_policies` table
   shaped on `withdrawal_policies`: `(tenant_id, brand_id, asset_code,
   operation, approval_threshold_minor_units, required_approvals,
   effective_from)`, **append-only** — `UPDATE` denied by trigger, a
   change is a new row (`withdrawal_policies_deny_update` precedent, whose
   own comment explains why: an editable threshold row leaves a loosened
   threshold in place with no trace). Two non-negotiable properties:
   - The threshold is denominated in the **same asset's minor units** as
     the operation's amount. Migration `0046` exists because this was got
     wrong once; `policy.go`'s own fallback comment explains the BTC
     failure mode (a threshold expressed in the wrong asset's units
     silently raises the effective four-eyes bar by orders of magnitude).
   - **Fail closed when no row resolves**: threshold `0`,
     `required_approvals` = 2 — everything requires full dual control.
     Identical to `internal/withdrawal/policy.go`'s
     `fallbackApprovalPolicy`.

### B1.3 RLS / tenancy design for the new bonus tables

Universal rules — every table below, with no exceptions:

- `tenant_id UUID NOT NULL`.
- `ENABLE ROW LEVEL SECURITY` **and** `FORCE ROW LEVEL SECURITY`. FORCE
  is the load-bearing half: the application role owns these tables and
  bypasses non-FORCE policies entirely.
- **No `BYPASSRLS` assumption anywhere in application code.** Every bonus
  read and write goes through `db.Pool.WithTenant` /
  `WithPrincipalScope` / `WithPlayerScope`. A bonus query issued on a
  bare pool connection reads zero rows under FORCE RLS and is therefore a
  silent-wrong-answer bug, not a loud one.
- `brand_id UUID` where brand narrowing applies, with a **composite
  foreign key `FOREIGN KEY (brand_id, tenant_id) REFERENCES
  brands (id, tenant_id)`** (migration `0043`'s precedent) plus
  `CHECK (brand_id IS NULL OR tenant_id IS NOT NULL)`. A plain
  `brand_id REFERENCES brands(id)` does **not** prevent tenant A's row
  from naming tenant B's brand; the composite FK is what makes
  cross-tenant brand attachment structurally impossible rather than
  policy-dependent.
- `player_account_id` is **never** accepted as an independent
  client-supplied id alongside `tenant_id`/`brand_id`. It is either
  composite-FK'd to `(player_account_id, tenant_id)` or populated by a
  trigger from an already-validated parent, mirroring
  `ledger_accounts_populate_from_wallet()`.
- A `BEFORE TRUNCATE ... FOR EACH STATEMENT` deny trigger.
- **No `FOR ALL` policy anywhere.** Policies are split per command, and
  `DELETE` gets a *visibility-only* policy or none at all — see the
  justification below.

#### Chosen policy shape, and why

**Precedent chosen: `asset_authorizations` as corrected by migration
`0047` Fix 3 (per-command split, no DELETE policy), combined with
`ledger_accounts`' two-policy dual scope for player-owned tables.**

Why that pairing rather than the obvious alternatives:

- **Not `risk_rules`' dual-scope (`tenant_id IS NULL OR tenant_id = ...`)
  shape**, because Wave 1 should have **no platform-wide Campaign**.
  `10-bonus-engine-architecture.md` §8 already records that a
  platform-wide Campaign is "likely reachable only via a platform-scoped
  write path that does not exist yet for any domain". Building the
  nullable-`tenant_id` policy arm now would create dead code that a later
  change could silently activate — a `tenant_id IS NULL` row is visible
  to *every* tenant, so a bug that writes one is a cross-tenant data
  exposure, not a cosmetic defect. **Recommendation: `tenant_id` is
  `NOT NULL` on every bonus table in Wave 1; platform-wide campaigns are
  an explicitly deferred future consideration.** This is also what makes
  `RolePlatformAdmin` holding no bonus permission consistent rather than
  a gap.
- **Not a `FOR ALL` policy**, because `0047` Fix 3 proved live what that
  costs: `FOR ALL` silently includes `DELETE`, and deleting a
  configuration row that encodes a *denial* silently widens eligibility,
  with no `BEFORE INSERT/UPDATE` trigger able to catch it (a DELETE fires
  neither). The bonus analogue is direct and has money attached: delete
  an Offer's excluded-game row, an eligibility exclusion, or a Segment's
  exclusion predicate, and the Offer silently becomes more generous than
  anyone approved.

Per table:

| Table (placeholder name) | Policy shape |
|---|---|
| `bonus_campaigns`, `bonus_offers`, `bonus_segments`, `bonus_codes`, `bonus_bulk_jobs`, `bonus_approval_policies`, `bonus_change_requests`, `bonus_change_approvals` | Staff-only, per-command: `tenant_isolation_read` (SELECT), `tenant_isolation_insert` (INSERT), `tenant_isolation_update` (UPDATE). **No DELETE policy.** Every predicate is `NULLIF(current_setting('app.player_account_id', true), '') IS NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid` |
| `bonus_grants`, `bonus_conversions` | `ledger_accounts`' dual scope, with the staff half split per command as above, plus `player_self_scope FOR SELECT USING (player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid)`. Player read only — never INSERT/UPDATE, for `withdrawal_requests`' stated reason: a player-writable row here is a complete four-eyes bypass |
| `bonus_progress` | Staff-only per-command policies **plus** `ledger_deny_mutation()` on `BEFORE UPDATE OR DELETE` (append-only, per §10.1's immutability requirement). **No `player_self_scope`** — see below |
| `bonus_adjustments` | Staff-only per-command; append-only (`ledger_deny_mutation()`); **no player policy at all** — it is the staff-intervention record |
| `bonus_suggestions` | Staff-only per-command, no player policy. Suggestions are a targeting/marketing artifact derived from player behaviour; a player-visible suggestion row leaks the operator's own segmentation model back to the player |

**`bonus_progress` has no player-read policy — this is deliberate and is
a constraint on `bonus-engine`'s API design, not an oversight.** The
Progress trail carries, per §10.1, the Risk and RG decision codes
consulted and the output of Bonus-Engine-owned abuse detectors (velocity,
device/payment fingerprint linking). A player who can read their own raw
Progress trail learns exactly which control they tripped and which they
did not, which is directly attack-useful for the bonus-abuse threat this
subsystem exists to counter. The player-facing
`GET /v1/me/bonuses/{grantID}` must therefore serve a **curated
projection** (state, amounts, remaining wagering requirement, expiry,
player-meaningful reason codes) computed server-side, never a passthrough
of the trail. Flagged for `bonus-engine` and `backend-platform`.

**Related, carried forward from doc 25's F15**: `bonus:read` must never
become a side channel around `verification:read`. The
`awaiting_verification` Grant status is a KYC-derived fact; a caller
holding `bonus:read` without `verification:read` must see a generalized
`pending`, not the KYC-specific value. The same rule applies to any
RG-derived status surfaced on a Grant — a `bonus:read` holder must not be
able to infer a self-exclusion from a Grant's blocked state.

### B1.4 Audit event catalogue

Every row writes an `audit.Record` entry **in the same transaction as the
action itself** (`internal/audit`'s existing contract — an action that
"succeeded" with no audit record is structurally impossible, not merely
unlikely). Action naming follows the live convention
`<domain>.<past_tense_event>` (`casino_bet.posted`,
`kyc.verification_status_changed`). `audit.Entry` has no dedicated
before/after fields, so before/after goes in `Metadata` under `before`
and `after` keys, as every existing domain already does.

| Action | Actor types | TargetType | Reason code | Before/after in Metadata |
|---|---|---|---|---|
| `bonus_campaign.created` | staff | `bonus_campaign` | No | after: full campaign definition |
| `bonus_campaign.updated` | staff | `bonus_campaign` | No | both |
| `bonus_campaign.version_published` | staff | `bonus_campaign` | No | both (prior version id → new version id) |
| `bonus_campaign.activated` | staff | `bonus_campaign` | **Yes** | both + `change_request_id`, approver ids, cost tier, threshold |
| `bonus_campaign.suspended` | staff, system | `bonus_campaign` | **Yes** | both (why a campaign was killed is the operational record) |
| `bonus_offer.created` / `bonus_offer.version_published` | staff | `bonus_offer` | Yes on publish above tier | both, incl. all five axes |
| `bonus_offer.deprecated` | staff | `bonus_offer` | No | both |
| `bonus_segment.created` / `.updated` / `.deleted` | staff | `bonus_segment` | No | **predicate only** — see the PII rule below |
| `bonus_code.issued` / `.revoked` | staff | `bonus_code` | Yes on revoke | both + max redemptions, aggregate value cap |
| `bonus_grant.issued` | system, staff | `bonus_grant` | Yes when staff-initiated | after + offer version id, amount, asset, trigger reference |
| `bonus_grant.activated` | system, player, staff | `bonus_grant` | Yes when staff-initiated | both |
| `bonus_grant.progressed` | system | `bonus_grant` | No | contribution delta + cumulative (see volume note) |
| `bonus_grant.completed` | system | `bonus_grant` | No | both |
| `bonus_grant.converted` | system, staff | `bonus_grant` | **Yes** when a staff override bypassed normal completion | both + ledger transaction group id, payout rule applied |
| `bonus_grant.cancelled` | player, staff | `bonus_grant` | **Yes, always** | both |
| `bonus_grant.forfeited` | system, staff | `bonus_grant` | **Yes, always** | both + the breach detected |
| `bonus_grant.expired` | system | `bonus_grant` | **Yes, always** | both |
| `bonus_grant.reversed` | system | `bonus_grant` | **Yes, always** | both + reversing event reference |
| `bonus_adjustment.applied` | staff | `bonus_adjustment` | **Yes, always** | both + amount, asset, `change_request_id`, approver ids, `threshold_at_decision` |
| `bonus_bulk_job.submitted` | staff | `bonus_bulk_job` | **Yes, always** | after + recipient-set hash, row count, per-player value cap |
| `bonus_bulk_job.executed` | staff, system | `bonus_bulk_job` | **Yes, always** | both + `change_request_id`, approver ids, success/failure counts |
| `bonus_change_request.filed` / `.approved` / `.rejected` / `.cancelled` | staff | `bonus_change_request` | Yes on reject/cancel | both + requester, approver, operation, payload |
| `bonus_approval_policy.written` | staff | `bonus_approval_policy` | **Yes, always** | both (superseded row → new row) — loosening a threshold must be as visible as using it |
| `bonus_config.changed` (engine configuration, tiers, feature toggles) | staff | `bonus_config` | **Yes, always** | both |
| `bonus_provider.correlated` (a provider-native / externally-fulfilled Grant's inbound status mapped to a Grant) | service | `bonus_grant` | No | provider id, provider reference, raw provider status string, mapped state |
| `bonus_suggestion.generated` / `.actioned` | system, staff | `bonus_suggestion` | Yes on action | both |

Every entry additionally carries `TenantID`, `ActorType`/`ActorID`,
`Outcome`, and — for any staff- or API-driven action —
`IPAddress`/`UserAgent`/`RequestID`, per CLAUDE.md's audit rule. A
**denied** attempt (RBAC refusal, four-eyes refusal, RLS refusal surfaced
as a not-found) writes an entry with `Outcome: denied`; a control that
fires silently leaves no evidence it fired.

**Volume note, flagged rather than decided**: `bonus_grant.progressed`
fires per qualifying settled round/bet and will dominate `audit_log` by
volume. `security`'s position is that the *Progress trail itself* is the
per-event record (it is already immutable, append-only, and carries the
same facts), and `audit_log` should receive a **per-transition**
aggregate rather than a per-contribution row — but this is a retention/
cost decision owned jointly with `bonus-engine` and `qa`, and it must be
decided explicitly. Whichever way it goes, no lifecycle *transition* may
be aggregated away.

#### What must never appear in a bonus audit record or Progress entry

Confirmed against the catalogue above: none of these events needs any of
the following, and any implementation that logs one is a blocking
finding.

- Passwords, password hashes, session tokens, refresh tokens, token
  hashes, JWTs, signing keys, or MFA/step-up challenge material.
- Provider API credentials, HMAC signing secrets, or per-tenant PSP /
  external-reward-provider credentials. A `bonus_provider.correlated`
  entry records the provider **id** and the provider **reference**, never
  the credential used to authenticate the callback.
- Raw KYC documents, document contents, document URLs, or
  KYC-vendor-supplied identity attributes. A Grant blocked pending
  verification records a status, not a reason derived from document
  content.
- Any PAN, card token, or payment instrument detail — a deposit-triggered
  bonus records the triggering deposit's internal id and
  `provider_tx_id`, never the instrument.
- Crypto private keys or signing material (structurally impossible per
  ADR 0008, restated so no future "just log the wallet address's key for
  debugging" reaches review).
- **Segment membership lists.** A `bonus_segment.*` audit entry records
  the segment's *predicate*, plus a content hash and a row count of the
  resolved membership — never the resolved list of player ids, emails, or
  any attribute used to resolve it. Same rule for a BulkJob's recipient
  set: the pinned approval payload carries a hash and a count, never the
  list. A membership list in an append-only 5–7-year-retention store is a
  PII copy that outlives every erasure request made against it
  (`docs/architecture/16-privacy.md`).
- **Full Risk/RG rule rows.** §10.1 already says the Progress trail
  records the decision's `Code`, not its internal detail. Restated as a
  security requirement: recording the rule's `Threshold` would put the
  tenant's own limit configuration into a record that a
  `bonus:read`-holding support agent — and, through any future
  player-facing dispute export, the player — can read.

### B1.5 Authorization and tenant-isolation tests every `qa` coverage must include

`security`-specified, per `.claude/agents/security.md`. These are
mandatory for the Bonus Engine's `qa` gate; a wave is not complete
without them.

- A request for tenant A's Campaign / Offer / Grant / Progress /
  Adjustment / BulkJob using tenant B's **valid** staff token returns
  403/404, never data — one case per table.
- A player JWT for player X cannot read player Y's Grant or Conversion,
  same tenant and cross-tenant.
- A player-scoped connection reads **zero** rows from `bonus_campaigns`,
  `bonus_offers`, `bonus_segments`, `bonus_codes`, `bonus_progress`,
  `bonus_adjustments`, `bonus_suggestions` (the
  `app.player_account_id IS NULL` half of each predicate).
- A `RoleTenantAdmin` token is denied every one of
  `bonus_campaign:create/update/activate`, `bonus_offer:manage`,
  `bonus_segment:manage`, `bonus_grant:issue/cancel`,
  `bonus_adjustment:write`, `bonus_bulk:execute` — **including** while
  holding `bonus_config:read`/`bonus:read`.
- A `RolePromotionsManager` token is denied `bonus_adjustment:write` and
  `bonus_bulk:execute`; a `RoleBonusOperations` token is denied
  `bonus_offer:manage` and `bonus_campaign:activate`.
- A `RoleFinance` token is denied every bonus permission.
- **The escalation test**: a `tenant_admin` caller attempting to create a
  `promotions_manager` or `bonus_operations` staff account is refused
  (403), and the refusal is audited. This is the §B1.1 P1 finding's
  regression guard.
- A four-eyes-required bonus operation with **one** approver fails; with
  the **same approver twice** fails; with **two staff accounts resolving
  to the same `person_id`** fails; with an approver whose `person_id` is
  NULL fails; with an approver whose `status <> 'active'` fails; with an
  approver from a different tenant fails.
- A request with one `approve` and one `reject` fails (a rejection is not
  overridable).
- A `bonus_change_requests` row cannot have its `payload`, `operation`,
  `target_id` or `amount_at_request` altered after insert; an `applied`
  request cannot be re-applied.
- An approved request for adjustment amount N cannot be consumed by an
  adjustment of amount N+1 (the JSONB payload match).
- An approved BulkJob request cannot be consumed by a job whose resolved
  recipient set hashes differently.
- **The upsert test (the `0047` AFTER-trigger regression guard)**: a
  dual-controlled bonus write performed as
  `INSERT ... ON CONFLICT DO UPDATE` consumes **exactly one** approval —
  not two, and not one-then-error.
- A `DELETE` against every bonus table removes zero rows; a `TRUNCATE`
  raises.
- Lowering a `bonus_approval_policies` threshold requires a new row (the
  `UPDATE` is refused) and is audited, and a decision recorded before the
  change still shows its original `threshold_at_decision`.
- With **no** `bonus_approval_policies` row for a tenant/asset/operation,
  every four-eyes-eligible operation requires full dual control (the
  fail-closed fallback), rather than none.
- A `bonus:read` holder without `verification:read` never receives the
  `awaiting_verification` status value.

### B1.6 What this section does not cover

It reviews no code, because none exists. It does not certify the Bonus
Engine as secure — it specifies what "secure enough to mark a wave
complete" will be measured against. Not in scope: the bonus ledger
postings themselves (`ledger-finance`, ADR 0032 — this section assumes
but does not verify their correctness), the Risk/RG call-site composition
order (`risk`/`identity-compliance`), penetration testing, and any
jurisdiction-specific regulatory security requirement.

### B1.7 Declared dependencies

1. **`bonus-engine`'s Wave-1 domain model — for exact table and column
   names.** Every table name in §B1.3 is a placeholder. Of the ten
   entities named in the Wave-1 brief, only Campaign, Offer, Grant and
   Progress are defined in `docs/architecture/10-bonus-engine-
   architecture.md`; **Conversion, Adjustment, Suggestion, Segment,
   BonusCode and BulkJob appear nowhere in any committed document**. The
   policy *shapes*, permission *semantics* and audit *events* above hold
   regardless of naming, but this section must be re-read against the
   real model before any migration is written — in particular whether
   Conversion is its own table or a Grant state, and whether Suggestion
   is player-derived (it is treated as such here, which is why it gets no
   player-read policy).
2. **`architect`'s cross-domain map** — for confirmation that (a) no
   platform-wide Campaign write path is required in Wave 1, (b) the
   `bonus_config:manage` split in §B1.1 is accepted as an amendment to
   `25-bonus-gamification-api-architecture.md` §2, and (c) the two new
   roles are the right count rather than one or three.
3. **`backend-platform` / `identity-compliance`** — the staff-creation
   allowlist and platform-scoped-caller restriction change in §B1.1's P1
   finding lives in `internal/httpserver/admin_routes.go`, which
   `security` does not own.
4. **`ledger-finance`** — confirmation that a bonus Adjustment is
   expressible as a compensating ledger posting (never a balance edit)
   and that the `bonus_expense` / `bonus_*` transaction types land before
   any adjustment path is wired, given HR-9's fail-closed posting guard.
5. **Human/product decision, still open** — the Terminal-Grant
   settlement-credit question (`10-bonus-engine-architecture.md` open
   question 7). Whichever of (a)/(b)/(c) is chosen determines whether a
   new audit action and a new four-eyes tier are needed here.

---

## Stage 4H-B1 Wave 1.5 (Fix Wave) — cross-domain security contract additions

Status: **DESIGN / CONTRACT ONLY — `NOT IMPLEMENTED`.** No code, no
migration, no permission constant, and no Human Decision Register item is
authorized or selected by this section. Produced for task **4HB1FW-04**.

This section is `security`'s own file and edits nothing else. Every
requirement that lands in another specialist's document or package is
stated here as a **named, routed requirement** (`REQ-…`) for that
specialist's own dispatch to adopt — `security` does not edit
`docs/architecture/10`, `30`, `31`, `32` or any migration (CLAUDE.md, "no
specialist redesigns shared architecture unilaterally").

### W15.0 What this section closes, and what it does not

| Item | Status after this section |
|---|---|
| **SEC-W15-03** (P0 — no actor≠beneficiary rule anywhere) | **Closed at the design level** by §W15.1 (invariant `SEP-1`). Closed in code only when each adopting domain implements it. |
| **SEC-W15-01** (P0 — affiliate four-eyes satisfiable by two colluding affiliate accounts) | **Closed at the design level** by §W15.2 (rule `AFF-4E-1`). |
| **DEP-AFF-1** (affiliate users as `StaffUser`s) | **Decided**: conditional accept, four binding conditions (§W15.2.2). |
| **DEP-CRM-4** (CRM permissions, blast radius, token design) | **Decided** (§W15.3). |
| **SEC-W15-09 item 3** (`pending_settlement` breaks §B1.2 item 7's predicate) | **Closed at the design level** by §W15.4.2. |
| **SEC-W15-20** (`BonusSuggestion` has no permission) | **Closed**: two permissions specified (§W15.4.3). |
| **SEC-W15-04** (EDR has no RLS / read-gating story) | **Closed at the design level** by §W15.5 (`EDR-S1`–`EDR-S5`). |
| **SEC-W15-02** (P0 — CRM per-player grant calls escape the bulk control) | **NOT closed by this section alone.** §W15.3's `CRM-BR-2` states the binding requirement; it is implementable only by `architect` (doc 31) and `bonus-engine` (doc 10 N2.4) in their own dispatches. It remains open until both adopt it. |
| SEC-W15-05 … SEC-W15-13 (the remaining P1s) | **Open.** Not addressed here; they are separate findings against `architect`'s and `bonus-engine`'s files. |
| **RK-W15P2-1** (P0, `risk` Phase 2 — `SEP-1` fails **open**, not closed, on a partial-RLS-truncation resolver read) | **Closed at the design level** by §W15.1.9 (cardinality assertion + step-0 tenant-scope self-proof), as corrected by §W15.1.9's second pass (fail-closed `IF/ELSIF/ELSE` replacing the original unmatched-`CASE`, plus the `ancestor_closure` shape's Step 0b totality proof) after `risk`'s partial re-open proved the first pass's `CASE` was silently inert on exactly the resolver shape (Affiliate's reflexive ancestor closure) the fix was written to catch. One dependency remains routed, not closed: `architect`'s confirmation that `agentnetwork` has no cross-tenant parent/child edge (§W15.1.9). This is a Fix Round 2 correction to §W15.1.3/§W15.1.4, which addressed only the *empty*-result failure mode. |
| **RK-W15P2-6 / RK-W15P2-7** (`risk` Phase 2 — the non-NULL-actor precondition is enforced by a pre-deploy query, not a DB constraint; the required denial-audit record is unimplementable inside a `BEFORE` trigger that aborts its own transaction) | **Closed at the design level** by §W15.1.10 (DB-level `CHECK` constraint, routed as `REQ-SEP-STAFF-1`) and §W15.1.11 (separate-transaction denial-audit record, per the ADR 0031 §39 precedent). §W15.1.10's constraint was **not** signed off as first drafted this round — `identity-compliance` found it invisible to platform-scoped (`tenant_id IS NULL`) staff on both the remediation side and the constraint's own applicability; closed by §W15.1.10's second pass (two-pass remediation, a confirmed-and-corrected `platform_admin` exemption, and the step-4 `IS DISTINCT FROM` fix that the exemption's correctness actually depends on). A residual limitation on the audit side is stated honestly in §W15.1.11 and is not claimed closed. |
| **NEW-6** (`code-reviewer`, doc 32 §6.5.2 — the affiliate ancestor-chain resolver is non-reflexive: fail-open on a same-node declared interest, deadlock on a flat/root node) | **Closed at the design level** by §W15.2.7 (reflexive ancestor closure). The corresponding edit to doc 32 §6.5.2 itself is `architect`'s, in a separate dispatch this round; this document's own restatement is corrected here so it does not perpetuate the non-reflexive wording in the meantime. |

Nothing in this section makes any subsystem "secure". It specifies what
three named controls must be, so that the implementations that come later
can be measured against something concrete rather than reviewed on vibes.

### W15.1 `SEP-1` — the actor ≠ subject/beneficiary invariant (closes SEC-W15-03)

One invariant, adopted at three (and later more) enforcement points. It is
deliberately **not** three domain-local rules: three separately-invented
comparisons is three places for the `IS NOT NULL AND` mistake of migration
`0029` to recur, and the whole reason this finding exists is that a
conditional comparison of exactly that shape shipped inert once already.

#### W15.1.1 Statement

> For any **economically consequential operation** `O`, with acting
> principal `A`, approver set `P`, and resolved beneficiary set `B(O)`:
> the platform **refuses** `O` if `person(A) ∈ persons(B(O))`, or if
> `person(p) ∈ persons(B(O))` for any `p ∈ P`. The platform **also
> refuses** if `person(A)`, any `person(p)`, or `persons(B(O))` cannot be
> fully resolved — an unresolvable identity on either side is a refusal,
> never a pass.

An **economically consequential operation** is defined by a property, not
by an enumeration, so that a new operation is in scope by default: *any
operation whose success causes, or authorizes, value to accrue to a
determinate party* — cash, bonus value, points, commission, a free-round
entitlement, or anything convertible into those. If a domain is unsure
whether an operation qualifies, it qualifies.

`SEP-1` is **unconditional and threshold-independent**. It is not gated by
`bonus_approval_policies`, by an affiliate threshold, by audience size, or
by any configuration. This is the core of SEC-W15-03: a size-based
four-eyes threshold is *inverted* for the self-deal vector, because a
self-deal is optimally executed at size 1 and at the smallest amount that
is worth taking — i.e. below every threshold a tenant would plausibly set.
`SEP-1` fires at amount 1 and at audience size 1.

`SEP-1` and four-eyes are **orthogonal, and neither substitutes for the
other**. Two independent approvers, neither of them the beneficiary, do
not help if the *requester* is the beneficiary and the approvers are
rubber-stamping a line item. Equally, `SEP-1` holding says nothing about
whether a second pair of eyes saw the amount.

#### W15.1.2 The three-part contract each adopting domain supplies

An adopting domain does not re-derive the rule. It supplies exactly three
things and reuses the shared mechanism for everything else:

1. **A beneficiary resolver.** A SQL function that, given the authorizing
   row, returns the set of `persons.id` values that benefit if the
   operation succeeds. It must be **total** (return a row set or raise —
   never return empty as a "no beneficiary" answer), **deterministic**,
   and evaluated **in the same transaction** as the authorizing write.
   An empty result is a refusal, not a pass: "this operation benefits
   nobody identifiable" is exactly as ineligible as "this operation
   benefits the actor", for the same reason migration `0034`'s doc comment
   already gives for an unresolvable approver.
2. **An enforcement point.** The row whose insertion *authorizes* the
   operation — the change-request row and the approval row — never the row
   that merely reports it afterwards.
3. **Nothing else.** The comparison, the resolution order, the NULL
   handling and the refusal semantics are the shared template below and
   are not a per-domain choice.

Resolvers, per adopting domain (these are the *requirements*; the owning
specialist writes them in their own file):

| Domain | Operation | `B(O)` resolves to | `resolver_shape` (§W15.1.9) |
|---|---|---|---|
| Bonus | Grant issuance, Grant activation | the Grant's `player_account_id → player_accounts.person_id` | `single_subject` |
| Bonus | Bonus adjustment, staff-forced conversion, manual release override | the target Grant's / target wallet's player account → `person_id` | `single_subject` |
| Bonus | `BulkGrantJob` execution | the **set** of persons behind the pinned recipient set (a set-membership test, not a scalar comparison) | `pinned_set` |
| Bonus | `BonusSuggestion` review and activation | the persons behind the resolved `proposed_player_population` | `pinned_set` |
| CRM | Journey/campaign activation containing an `offer_request` step | the persons behind the pinned, resolved audience. A size-1 audience is precisely the self-deal case and is covered by construction | `pinned_set` (audience size 1 is a `pinned_set` of one, not `single_subject` — its count still comes from the pinned audience, not from structure) |
| Affiliate | `CommissionApproval`, `CommissionSettlementInstruction`, re-attribution, agreement/rule-version activation | the affiliate node's **reflexive ancestor closure** — the node **itself**, together with its ancestor chain — under any sub-affiliate override agreement (doc 32 §6.4 — a parent node benefits from a child's accrual, so the parent's people are beneficiaries too, **and** the node's own people are beneficiaries of its own accrual, which a strict ancestors-only reading silently excluded; corrected by Fix Round 2, §W15.2.7, closing `NEW-6`), expanded to that closure's affiliate-account persons **plus** its declared beneficial-interest persons (§W15.2.5) | `ancestor_closure` — no pinned count exists for this shape; see §W15.1.9's second pass (Step 0b + `SECURITY DEFINER` construction), routed dependency on `architect` re cross-tenant `agentnetwork` edges |

The set cases (bulk, audience) must test membership against the
**already-pinned, materialized** recipient/audience set that §B1.2 item 2
requires be hashed into the approval payload — never against a
re-resolution at execution time. Re-resolving would both cost a second
full scan and reintroduce the set-swap vector the pinning exists to close.

#### W15.1.3 The database mechanism, exactly

**Precedent reused: migration `0034_stage3d_withdrawal_governance.up.sql`,
function `withdrawal_approvals_enforce_governance()`.** That function's
*person-resolution shape* is the template — it is the one in this codebase
that survived review, and §B1.2 item 3 already mandates it for the bonus
four-eyes trigger. `SEP-1` lives in the same trigger family and is written
as a sibling of that function, not as a new pattern:

- One function per adopting authorizing table, named
  `<table>_enforce_separation()`, `BEFORE INSERT` (and `BEFORE UPDATE`
  where the authorizing row's actor/approver fields are settable).
- `BEFORE` is correct **here specifically** because this trigger is *pure
  validation with no side effect* — it consumes nothing and writes
  nothing. That is exactly the carve-out §B1.2 item 5 preserved when it
  banned `BEFORE` for dual-control *consume* logic (migration `0047`'s
  correction to `0044`). The `SEP-1` trigger being `BEFORE` and the
  four-eyes consume trigger being `AFTER` is deliberate, not an
  inconsistency: firing twice on an `INSERT … ON CONFLICT DO UPDATE`
  costs a duplicated comparison, which is harmless; firing a *consume*
  twice is not.
- Body, in order — each step a refusal, no step conditional on the next:
  1. Resolve the actor principal to a `staff_users` row. `NOT FOUND` →
     **refuse**. There is no service-identity carve-out on any
     value-moving operation (§B1.2 item 3 already states this for bonus);
     the only service-identity actor anywhere in this contract is
     `bonus_suggestion:create`, which moves no value and is not an
     economically consequential operation.
  2. `person_id IS NULL` → **refuse**.
  3. `status <> 'active'` → **refuse**.
  4. `staff_users.tenant_id IS DISTINCT FROM` the authorizing row's
     `tenant_id` → **refuse** (the tenant-scoped analogue of `0044`'s
     platform-scope check). **`IS DISTINCT FROM`, not bare `<>` — Fix
     Round 2, second pass (`identity-compliance`'s `REQ-SEP-STAFF-1`
     sign-off, §W15.1.10).** Every authorizing table in this contract is
     tenant-owned (`CLAUDE.md`: "every tenant-owned table carries
     `tenant_id`"), so the authorizing row's `tenant_id` is always a real
     UUID, never NULL — but the *actor's* side can be NULL, for exactly
     one legitimate reason: a `platform_admin` `staff_users` row, which
     migration `0011`'s own `CHECK` makes `tenant_id IS NULL` if and only
     if `role = 'platform_admin'`. Plain SQL `<>` against a NULL operand
     evaluates to NULL, and PL/pgSQL's `IF` treats a NULL condition as
     false — so a bare `<>` here would silently **not** refuse a
     `platform_admin` actor regardless of which tenant the operation
     belongs to, which is the identical "unresolvable side skipped instead
     of the statement aborting" defect step 5's own `IS NOT NULL AND`
     discussion (above) already forbids, recurring one step earlier in the
     same function. `IS DISTINCT FROM` is NULL-aware — `NULL IS DISTINCT
     FROM <any real UUID>` is `TRUE` — so a `platform_admin` actor is
     refused at this step on **every** tenant-owned authorizing row,
     unconditionally, consistent with §W15.1.1's "unresolvable is a
     refusal, never a pass." This is not a new restriction invented here;
     it is what step 4 already claimed to do and did not, as literally
     specified, actually do.
  5. Resolve `B(O)` via the domain's resolver. **Empty set → refuse. Any
     element whose `person_id` is NULL → refuse.**
  6. Compare **unconditionally**: `person(actor) = ANY(persons(B(O)))` →
     **refuse**. Same comparison, same function, for every approver.

**The one line that must be inverted relative to the precedent.** Migration
`0034` line 117 reads:

```sql
IF requester_person_id IS NOT NULL AND requester_person_id = approver_person_id THEN
```

The `IS NOT NULL AND` guard on the *subject* side is precisely the shape
that made migration `0029`'s check inert, and `SEP-1` must not reproduce
it. In a `SEP-1` trigger, a NULL on the subject/beneficiary side raises at
step 5 **before** the comparison is reached, so the comparison itself is
never guarded. Any implementation in which a NULL beneficiary person
causes the comparison to be skipped rather than the statement to abort is
a **blocking** finding, regardless of how the tests read.

**Two dependencies of the mechanism that already exist and must not be
weakened:**

- `staff_users_person_id_append_only()` (migration `0034`). `SEP-1` is
  only non-repudiable because a staff account's person linkage cannot be
  changed once set. If that trigger is ever relaxed, `SEP-1` becomes
  evadable by relinking immediately before acting.
- The pre-deploy verification §B1.2 item 3 already specifies —
  `SELECT id, email FROM staff_users WHERE tenant_id = <t> AND status =
  'active' AND person_id IS NULL` must return zero rows per tenant —
  becomes load-bearing for `SEP-1` too, because `SEP-1` refuses on a NULL
  actor person.

#### W15.1.4 `SEP-1-H1` — the inert-control hazard, and the test that catches it

`persons` carries RLS (migration `0015`) and every table in this contract
carries `FORCE ROW LEVEL SECURITY`. Migration `0048`'s defect was exactly
this: a `SELECT` inside a guard read zero rows because the connection set
no tenant context, so the guard was silently inert while passing every
test that asserted a legitimate operation succeeds.

A `SEP-1` resolver that reads zero rows refuses (step 5 treats empty as a
refusal), so the *failure mode is fail-closed rather than fail-open* —
which is the right direction, but it converts a security control into an
availability outage, and the pressure to "fix" it fast is precisely the
pressure that produces a `NO FORCE` toggle (§B1.0, which forbids that
absolutely, for any reason).

Required, therefore:

- The resolver executes under the operation's own tenant context — which
  is the case for a runtime write, unlike a migration — or is
  `SECURITY DEFINER` with an **explicit tenant predicate in the query
  itself**, never relying on the definer's RLS bypass to be scoped by
  something else.
- **The acceptance test set must include a positive case that proves the
  comparison actually fires** — a staff actor deliberately made the
  beneficiary, asserted to be refused *with the `SEP-1` error message*.
  A control that is permanently inert passes every negative test (every
  legitimate operation succeeds) and every "no self-deal happened" test.
  Only an adversarial positive case distinguishes "working" from
  "structurally unable to fire". This is the `0044`/`0047` lesson stated
  as a test obligation rather than a war story.

#### W15.1.5 First-degree household / linked accounts — **detection only, never a block**

Deterministic `Person` identity (`person_id` equality) **blocks**.
Probabilistic `Person` *similarity* — shared residential address, shared
device fingerprint, shared payment instrument, shared IP, a fuzzy identity
match below confidence 1, a declared-but-unverified relationship —
**detects**, and must never refuse an operation.

Reasoning, stated because this will be argued the other way under
pressure:

- Identity-resolution confidence is not a basis for refusing a legitimate
  grant. A false positive and a correctly-firing control are
  indistinguishable to the operator at the point of refusal, and the
  refusal carries no review path.
- A hard block on probabilistic linkage creates direct operational
  pressure to loosen the matcher. The same matcher serves AML and
  multi-account detection; degrading it to unblock a promotions workflow
  is a strictly worse outcome than not blocking in the first place.
- Small operators have genuine, lawful cases — a support agent issuing a
  goodwill grant to a player who happens to share a household — that a
  hard block silently kills.

What detection must do instead:

- Emit the signal to **`bonus-engine`'s abuse detector** (doc 10 §1.4 and
  its device/payment-fingerprint linking), which is the domain that
  already owns this class of judgement. It must **not** become an
  affiliate-local, CRM-local or security-local heuristic — doc 32 §9.1 and
  doc 30 §8.3 already record the "second risk engine" failure this avoids.
- Raise a review item workable under `bonus_grant:review`.
- Attach a **reason code** to the Grant's Progress trail, never the
  underlying attributes (§B1.4's PII rule: no addresses, no fingerprints,
  no instrument details in an append-only 5–7-year store).
- Never auto-reverse, auto-forfeit, or auto-suspend on the signal alone.

#### W15.1.6 The primitive this reuses, and its dependency

`SEP-1` builds on exactly one existing primitive and invents none:

- `staff_users.person_id` (migration `0029`, made append-only and
  mandatory-for-approval by migration `0034`), ↔
- `player_accounts.person_id` (migrations `0009`/`0010`), the
  `PlayerAccount → Person` link,

with `persons` as the single comparison space. This is the same linkage
the withdrawal four-eyes self-approval check already uses; `SEP-1`
generalizes it from "approver ≠ withdrawing player" to "no actor and no
approver is any beneficiary of any value-moving operation".

**Declared dependency (not blocking this design, flagged honestly):**
`identity-compliance`'s parallel task 4HB1FW-05 is confirming that this is
the right primitive and defining the affiliate identity/authority
boundary. This section is designed against the interface as `security`
understands it today. If `identity-compliance` introduces a different
canonical person-resolution entry point (for example a function rather
than a direct join, or a confidence-carrying resolution), `SEP-1`'s
*resolver implementation* changes and its *semantics* do not — with one
exception that must be watched: if the canonical resolver ever returns a
probabilistic match, `SEP-1` must consume only its deterministic arm, per
§W15.1.5. That is a requirement on the interface, routed as
**REQ-SEP-ID-1**.

#### W15.1.7 Routed adoption requirements

| ID | Owner | Requirement |
|---|---|---|
| **REQ-SEP-BONUS-1** | `bonus-engine` | Adopt `SEP-1` at Grant issuance/activation, adjustment, forced conversion, and `BulkGrantJob` execution. Supply the four resolvers in §W15.1.2. |
| **REQ-SEP-BONUS-2** | `bonus-engine` | Adopt `SEP-1` at `BonusSuggestion` review and activation (§W15.4.3). |
| **REQ-SEP-BONUS-3** | `bonus-engine` | Own the household/linked-account **detection** path of §W15.1.5; refuse any request to make it a block. |
| **REQ-SEP-CRM-1** | `architect` (doc 31) | Adopt `SEP-1` at journey/campaign activation containing an `offer_request` step, against the pinned audience. |
| **REQ-SEP-AFF-1** | `architect` (doc 32) | Adopt `SEP-1` at `CommissionApproval`, settlement instruction, re-attribution and agreement/rule activation, with the **reflexive** ancestor-closure beneficiary set of §W15.1.2 (node itself plus ancestors — not ancestors alone; §W15.2.7). |
| **REQ-SEP-ID-1** | `identity-compliance` | Confirm the `staff_users.person_id` ↔ `PlayerAccount → Person` primitive; if a canonical resolver is introduced, expose a deterministic-only arm for `SEP-1`. |

#### W15.1.8 Tests `SEP-1` requires, in every adopting domain

- A staff actor whose `person_id` equals the beneficiary player's
  `person_id` is refused, at **amount 1** and at **audience size 1** —
  i.e. below every configured threshold. (The core SEC-W15-03 case.)
- The same, where the actor and the player are **two different accounts**
  resolving to **one `person_id`**.
- The same for an **approver** rather than the actor, on an operation
  whose requester is clean.
- A bulk/audience operation whose recipient set **contains** the actor's
  person among thousands of others is refused — set membership, not just
  the scalar case.
- An operation whose beneficiary set resolves **empty** is refused.
- An operation where a beneficiary's `person_id` is **NULL** is refused
  (the migration-`0034`-line-117 inversion regression guard).
- An actor whose own `person_id` is NULL, whose `status <> 'active'`, or
  whose `tenant_id` differs from the operation's, is refused.
- **The anti-inertness test (`SEP-1-H1`)**: the refusal cases above assert
  the specific `SEP-1` error, not merely "an error"; and a legitimate
  operation with a disjoint beneficiary set **succeeds**, proving the
  trigger is not refusing everything.
- Concurrency: two approvals racing on the same request, one of them
  self-dealing, never both commit.
- A `SEP-1` refusal aborts the authorizing transaction (no partial row
  survives in the change-request/approval table), **and** a
  separate-transaction denial-audit record with `Outcome: denied` and a
  `SEP-1`-specific reason code is written and is mechanically
  distinguishable, by `Code`, from an ordinary four-eyes policy decline —
  per §W15.1.11, which corrects this bullet's original, unimplementable
  "writes an audit record with `Outcome: denied`" claim (`RK-W15P2-7`).

#### W15.1.9 Fix Round 2 (`RK-W15P2-1`) — the cardinality assertion and step-0 tenant-scope self-proof (second pass: fail-closed default + the `ancestor_closure` shape)

**The finding, restated precisely.** §W15.1.3's step 5 ("Resolve `B(O)`
… Empty set → refuse. Any element whose `person_id` is NULL → refuse.")
and §W15.1.4's `SEP-1-H1` analysis together cover exactly two failure
shapes of the resolver's `SELECT`: **zero rows**, and **a NULL
`person_id` on a returned row**. Neither covers a third, more dangerous
shape: the resolver's join path crosses a table whose RLS policy is
scoped differently from the authorizing row — a beneficiary's
`player_accounts` row hidden by a stricter scope than the trigger's own,
or a recursive affiliate-ancestor walk (§W15.2.7) whose recursive term
crosses a node the connection's scope cannot see — and the query
returns a **non-empty, non-NULL, but truncated** `B(O)`: some genuine
beneficiaries are silently absent, not represented by an empty set or a
NULL row. Step 5 as originally specified does not fire on this shape at
all, because it is neither the empty case nor the NULL case, and step 6
then compares the actor only against the *rows that survived* — so a
self-dealing actor whose own beneficiary row was the one RLS silently
dropped is compared against an incomplete set and passes. This is a
genuine fail-**open**, not merely an availability problem, and it is
`risk`'s correctly-identified gap in `SEP-1-H1`'s own reasoning: `SEP-1-H1`
proved the *empty*-result case is fail-closed and stopped there.

**Fix part 1 — step 0, a tenant-scope self-proof, ported directly from
`internal/risk/evaluator.go`'s `verifyConnectionScope` /
`ErrTenantScopeMismatch` / `ErrPlayerScopedConnection`.** Every
`<table>_enforce_separation()` trigger runs this **before** step 1 of
§W15.1.3, not as a replacement for the "run under the operation's own
tenant context" mitigation but as its enforcement — a mitigation that is
only ever "the caller opened the right kind of transaction" is exactly
the discipline-not-database pattern `evaluator.go`'s own doc comment
(`ErrTenantScopeMismatch`) was written to eliminate for Risk, and `SEP-1`
must not reintroduce it one layer up:

```
-- Step 0 (NEW, Fix Round 2) — tenant-scope self-proof.
-- Direct port of internal/risk/evaluator.go:verifyConnectionScope.
scoped_tenant := NULLIF(current_setting('app.tenant_id', true), '')
scoped_player := NULLIF(current_setting('app.player_account_id', true), '')

IF scoped_player IS NOT NULL THEN
  RAISE EXCEPTION USING ERRCODE = 'SP001',
    MESSAGE = 'SEP-1: refuse — app.player_account_id is set on this
               transaction (ErrPlayerScopedConnection analogue)';
END IF;

IF scoped_tenant IS NULL THEN
  RAISE EXCEPTION USING ERRCODE = 'SP001',
    MESSAGE = 'SEP-1: refuse — app.tenant_id is not set on this
               transaction (ErrTenantScopeMismatch analogue)';
END IF;

-- parse as uuid; a non-uuid value refuses, same ERRCODE, same message
-- shape as evaluator.go's own "is not a uuid" branch.

IF scoped_tenant::uuid <> NEW.tenant_id THEN
  RAISE EXCEPTION USING ERRCODE = 'SP001',
    MESSAGE = format('SEP-1: refuse — transaction scoped to %s, row is
               for %s', scoped_tenant, NEW.tenant_id);
END IF;
```

This step proves the connection the resolver is about to run its
`SELECT` on is scoped to exactly the authorizing row's own tenant and
carries no player scope — closing the RLS-mismatch precondition rather
than only reacting to its symptom.

**Fix part 2 — step 5 is amended to a cardinality assertion, not a
bare emptiness/NULL check.** The resolver must return, per subject it
was asked to resolve, at most one non-NULL `person_id` row, and the
trigger must verify the **count** of rows returned equals the **count of
subjects the authorizing row claims to have**, not merely that the count
is nonzero.

**Fix Round 2, second pass (`RK-W15P2-1` partial re-open, `risk`).** As
first drafted this round, `expected_count` was computed by a SQL `CASE`
**expression** with exactly two `WHEN` branches and no `ELSE`. An
unmatched SQL `CASE` expression evaluates to `NULL`, and PL/pgSQL's `IF`
treats a `NULL` condition as false — so `IF resolved_count <>
expected_count THEN` silently never fires for any resolver shape not one
of the two enumerated, which is exactly the "unresolvable side skipped
instead of the statement aborting" defect this whole document keeps
finding and re-finding in different guises (migration `0029`'s inert
guard, step 4's bare `<>` just corrected above, and now this). `risk`
identified a live instance, not a hypothetical one: Affiliate's
**reflexive ancestor-closure** resolver (`REQ-SEP-AFF-1`, §W15.1.2, doc 32
§6.5.2) is a recursive graph walk with no pre-materialized pinned count —
it matches neither `WHEN` branch, so `expected_count` was `NULL` and the
one failure mode this section exists to catch (a truncated recursive
walk) was the one case it did not catch. Fixed as follows, replacing the
SQL expression with an explicit, fail-closed PL/pgSQL statement and
naming three resolver shapes instead of two:

```
-- Step 5 (AMENDED, Fix Round 2, second pass).
-- resolver_shape is a value the adopting domain's resolver declares as
-- part of the three-part contract (§W15.1.2 item 1, amended below) —
-- 'single_subject' | 'pinned_set' | 'ancestor_closure' — never inferred
-- from the authorizing row's shape by the trigger itself.

IF resolver_shape = 'single_subject' THEN
  expected_count := 1;

ELSIF resolver_shape = 'pinned_set' THEN
  -- the row count already pinned into the approval payload at §B1.2
  -- item 2 / CRM-BR-1 — the same content-hash-plus-row-count the
  -- set-swap control already requires; where an
  -- EconomicOperationIdentity exists for the operation (doc 34 §2.2),
  -- this is its subject_set_count field, not a second number invented
  -- here
  expected_count := <pinned subject_set_count>;

ELSIF resolver_shape = 'ancestor_closure' THEN
  -- No independent pinned or structural count exists for this shape —
  -- see "The ancestor-closure shape" below. Cardinality is not the
  -- control for this branch; totality proven *before* the resolver
  -- runs (Step 0b) is. expected_count is defined as resolved_count so
  -- that the comparison below is a no-op by construction for this
  -- shape, not a silently-vacuous NULL by omission.
  expected_count := resolved_count; -- assigned after the resolver call below

ELSE
  RAISE EXCEPTION USING ERRCODE = 'SP001',
    MESSAGE = format('SEP-1: refuse — unrecognized resolver_shape %L;
               a new resolver shape must add an explicit branch here
               (with either a pinned count or a Step-0b-style totality
               proof) before it may be deployed', resolver_shape);
END IF;

SELECT count(*), count(*) FILTER (WHERE person_id IS NULL)
  INTO resolved_count, null_count
  FROM <domain_resolver_function>(NEW. ...);

IF resolver_shape = 'ancestor_closure' THEN
  expected_count := resolved_count; -- see ELSIF above
END IF;

IF resolved_count = 0 THEN
  RAISE EXCEPTION USING ERRCODE = 'SP001',
    MESSAGE = 'SEP-1: refuse — beneficiary set resolved empty';
END IF;

IF null_count > 0 THEN
  RAISE EXCEPTION USING ERRCODE = 'SP001',
    MESSAGE = 'SEP-1: refuse — a beneficiary resolved to a NULL
               person_id';
END IF;

IF resolved_count <> expected_count THEN
  RAISE EXCEPTION USING ERRCODE = 'SP001',
    MESSAGE = format('SEP-1: refuse — resolver returned %s persons for
               %s expected subjects (partial resolution)',
               resolved_count, expected_count);
END IF;
```

This is now a PL/pgSQL `IF/ELSIF/.../ELSE` statement, not a SQL `CASE`
expression, precisely so that "a resolver shape nobody enumerated" is a
loud, distinct `RAISE EXCEPTION` — refused for being unrecognized, with
its own message, not a silent `NULL` that happens to make the next `IF`
false. The `single_subject`/`pinned_set` branches behave exactly as
before: a resolver whose join lost rows to a mismatched RLS scope
returns `resolved_count < expected_count` and is refused **on that basis
alone**, independent of whether the missing row happened to be the
actor's own.

**The ancestor-closure shape needs a different control, not a bigger
`CASE`.** The finding's own options were: (a) make the resolver
structurally total by construction, so there is nothing left to check by
count; or (b) compute an independent expected count for this shape too,
e.g. a separate non-recursive query that counts the ancestor chain's
length before the recursive walk runs it. **(b) is unsound here and is
rejected, not merely deprioritized.** Counting "the ancestor chain's
length" is not actually independent of the recursive walk it is meant to
check — an ancestor chain has no length known in advance except by
walking parent pointers up the tree, which is the same recursive
traversal, over the same rows, under the same connection scope, as the
resolver itself. If the connection's RLS scope truncates the walk at
node K, a "count the chain first" query hits the identical wall at the
identical node and returns the identical, equally-wrong number — the two
numbers agree with each other while both are wrong, and the cardinality
check passes on a truncated result. This is the exact shape of unsound
double-check the finding warned against checking for before picking (b),
and it fails that check.

**(a) is the fix: prove the resolver's connection is structurally unable
to truncate, before it runs, rather than counting after.** Two parts,
both required together:

1. **Step 0b (NEW) — subtree-scope self-proof, required specifically
   before any `ancestor_closure`-shaped resolver runs, in addition to
   (never instead of) Step 0's tenant-scope self-proof above:**

   ```
   -- Step 0b — only for resolver_shape = 'ancestor_closure'.
   scoped_node := NULLIF(current_setting('app.hierarchy_node_id', true), '')

   IF scoped_node IS NOT NULL THEN
     RAISE EXCEPTION USING ERRCODE = 'SP001',
       MESSAGE = 'SEP-1: refuse — app.hierarchy_node_id is set on this
                  transaction; the ancestor-closure resolver requires an
                  unnarrowed, whole-tenant connection to prove its walk
                  is total (WithNodeScope analogue, doc 32 §3/AFF-C2) —
                  a subtree-scoped connection cannot see the ancestors
                  above its own node and would silently truncate the
                  walk';
   END IF;
   ```

   `app.hierarchy_node_id` (doc 32 §3, `AFF-C2`, `DEP-AFF-5`) is the RLS
   dimension that scopes a connection to one node's own subtree —
   *descendants*, not ancestors. A connection narrowed by it is exactly
   the shape that would truncate an upward ancestor walk, the same way a
   narrower tenant scope would. Refusing outright when it is set (rather
   than trying to clear it and proceed) keeps this proof the same shape
   as Step 0: a precondition checked and refused on, never a value
   silently overridden and trusted.

2. **The `ancestor_closure` resolver function itself is `SECURITY
   DEFINER`, with the authorizing row's own already-tenant-proven
   `tenant_id` (from Step 0 — not a session GUC) compiled into an
   explicit predicate on every level of the recursive CTE, base case and
   recursive term alike** — the same discipline §W15.1.4 already requires
   of any `SECURITY DEFINER` resolver ("never relying on the definer's
   RLS bypass to be scoped by something else"). This is what makes the
   walk *cross-tenant-safe* rather than merely subtree-safe: `SECURITY
   DEFINER` bypasses RLS entirely (including any future RLS dimension
   that is not `app.hierarchy_node_id`), so the walk's only tenant
   boundary is the one written into the query text, not one enforced by
   whatever policy happens to be attached to the table. Combined with
   Step 0b, the resolver runs with a **proven**, whole-tenant,
   single-tenant view of exactly the authorizing row's own tenant, and
   `agentnetwork`'s node instances being `tenant_id NOT NULL` (doc 32 §3:
   "every node instance is `tenant_id NOT NULL`") means there is no row
   for the walk to lose within that tenant — the resolver is total by
   construction, not by having survived a count.

   Given that, `expected_count := resolved_count` for this shape (shown
   above) is not a gap being papered over: it is the correct expression
   of "this shape's totality is Step 0b's job, and Step 0b already
   refused if it could not be proven" — the cardinality comparison is
   structurally a no-op here because the thing it would have caught
   cannot occur once Step 0b holds.

   **Dependency stated, not assumed — routed to `architect` (doc 32/
   `agentnetwork` owner).** This construction depends on `agentnetwork`
   parent/child edges never crossing a tenant boundary — i.e., a node's
   ancestor chain, walked via parent pointers, never leaves the node's
   own tenant. Doc 32 §3 states every node *instance* is single-tenant,
   which this reasoning was checked against, but does not state as a
   standalone invariant that no *edge* connects nodes of two different
   tenants (relevant to the hybrid B2B licensing model, `docs/decisions/
   0006-…`, where a partner tenant's own sub-affiliates could in
   principle be modeled as rolling up cross-tenant). **`architect` must
   confirm this invariant holds for `agentnetwork` as built, or, if a
   cross-tenant edge is ever legitimate, the single-`tenant_id` predicate
   above is insufficient and the resolver needs a tenant-closure
   predicate instead of a tenant-equality one.** Until confirmed,
   Affiliate's adoption of `SEP-1` (`REQ-SEP-AFF-1`) must not ship against
   a hierarchy where this is unverified.

**Amendment to the three-part contract (§W15.1.2 item 1).** A beneficiary
resolver is no longer sufficient as "a SQL function that … returns the
set of `persons.id` values" alone; it must additionally declare its
**`resolver_shape`** (`single_subject` | `pinned_set` | `ancestor_closure`
— an adopting domain introducing a genuinely new shape adds a new named
branch to Step 5 above, per its fail-closed `ELSE`, before deploying it)
and be **verifiably total** for that shape — for `single_subject` this is
structural (there is exactly one target), for `pinned_set` it is the row
count **already** pinned at approval time (§B1.2 item 2, `CRM-BR-1`), and
for `ancestor_closure` it is Step 0b plus the `SECURITY DEFINER`
explicit-tenant-predicate construction above. This is not a new
obligation on adopting domains beyond what they already must pin or
prove; it is a requirement that the trigger **read and check** (or, for
`ancestor_closure`, **structurally foreclose**) a gap, rather than
trusting the join's row count implicitly.

**Honest scope of what this closes.** This closes the *partial-RLS-read*
shape of fail-open for all three named shapes, including the one
(`ancestor_closure`) that the first pass of this fix missed. It does
**not** by itself prove every possible resolver query is written
correctly — a resolver that never crosses a differently-scoped table has
nothing to truncate, and a resolver whose own logic (not RLS) omits a
genuine beneficiary is a resolver-authoring defect this mechanism cannot
detect, because the resolver itself decided what "expected" means (or,
for `ancestor_closure`, what "total" means) for anything beyond the
pinned-count set case. `qa`'s per-domain adversarial test set (§W15.1.8,
amended below) is what catches that class, one adopting domain at a
time; this section closes the *mechanism-level* gap, not every possible
resolver bug. Nor does it prove `agentnetwork` never grows a cross-tenant
edge — that is the routed dependency to `architect` stated above, and
this fix is not claimed complete for `REQ-SEP-AFF-1` until it is
confirmed.

**New tests, added to §W15.1.8's list:**

- A resolver whose join is made to cross a row outside the acting
  connection's RLS scope (simulated by scoping the trigger's own
  connection one tenant narrower than a beneficiary's actual row) returns
  fewer rows than `expected_count` and is refused with the
  cardinality-mismatch reason — asserted as its own distinct case from
  the pre-existing empty-set and NULL-person tests, for the
  `single_subject` and `pinned_set` shapes.
- **(New, second pass)** A trigger invoked with a `resolver_shape` value
  that matches none of the enumerated branches is refused with the
  distinct "unrecognized resolver_shape" error, not silently passed —
  the direct regression test for the defect this second pass fixes, and
  the only test that would have caught the original unmatched-`CASE`
  shape.
- **(New, second pass)** An `ancestor_closure` resolver invoked on a
  connection with `app.hierarchy_node_id` set is refused at Step 0b, with
  the subtree-scope-self-proof error, **before** the resolver runs at
  all — asserted by observing the resolver function is never invoked
  (e.g. via a call-count instrumentation on the test double), not merely
  by observing the overall refusal, so a future change that clears the
  GUC instead of refusing on it cannot pass this test by accident.
- **(New, second pass, blocked on the routed `architect` confirmation
  above)** If `agentnetwork` ever permits a cross-tenant parent/child
  edge, a beneficiary on the far side of that edge must still be resolved
  or the operation refused — this test cannot be written until
  `architect` confirms whether the case exists.

#### W15.1.10 Fix Round 2 (`RK-W15P2-6`, part a) — the non-NULL-actor precondition as a DB constraint

**The finding.** §W15.1.3's dependency list states the non-NULL-actor
precondition is upheld by "the pre-deploy verification §B1.2 item 3
already specifies" — a `SELECT` run once, by an operator, before a
tenant's promotions staff can file or approve anything. That is
"discipline in application/operational process," precisely the pattern
`CLAUDE.md` rules out ("enforced by PostgreSQL row-level security bound
to a connection-level setting — not by discipline in application code");
a pre-deploy query proves the precondition held **at the moment someone
remembered to run it**, and says nothing about a `staff_users` row that
is updated to `status = 'active'` with a NULL `person_id` five minutes
later, by any path that does not go through whatever created the
original account.

**Fix — a real, permanent, database-enforced constraint on
`staff_users`, replacing the query as the enforcement mechanism.**

**Fix Round 2, second pass — `identity-compliance`'s sign-off findings on
`REQ-SEP-STAFF-1`, checked against the real migrations
(`0011_create_staff_users.up.sql`, `person_id` added by `0029`, made
append-only by `0034`).** The constraint as first drafted this round was
**not** signed off, on two grounds, both addressed below:

**(1) Confirming `identity-compliance`'s reading of `SEP-1`'s own step 4
against `platform_admin` rows — this is `SEP-1`'s semantics, so the call
is `security`'s to make, and it is confirmed correct, with one
correction to how step 4 was itself specified.** Migration `0011`'s own
`CHECK` makes `role = 'platform_admin' ⟺ tenant_id IS NULL`, and every
`SEP-1`-authorizing table is tenant-owned (`tenant_id NOT NULL`,
`CLAUDE.md`). `identity-compliance` read step 4 (§W15.1.3) as refusing
every `platform_admin` actor unconditionally, on every such table, because
its `tenant_id` is never equal to any real tenant's id — and therefore a
`platform_admin`'s `person_id` buys `SEP-1` nothing, since it never
reaches step 5 regardless. **That reading is correct only for a
NULL-aware comparison, and step 4 was specified with a bare `<>`, which
is not one** — see the correction just made to step 4 above (`IS
DISTINCT FROM`, not `<>`), found while confirming this exact reading:
as originally written, a bare `<>` against `staff_users.tenant_id IS
NULL` evaluates `NULL`, `IF` treats that as false, and a `platform_admin`
actor would have silently **passed** step 4 on every tenant, not been
refused by it — the opposite of what `identity-compliance`'s reasoning,
and the sign-off decision below, depend on. With step 4 corrected to
`IS DISTINCT FROM`, `identity-compliance`'s reading now holds as stated:
a `platform_admin` actor is refused at step 4 on every tenant-owned
authorizing row, unconditionally, so requiring `person_id` of a
`platform_admin` row buys `SEP-1` nothing, and directly undercuts
migration `0033`'s documented "`person_id` is optional and NULL-forever
for the overwhelming majority of staff" invariant for no compensating
gain. The exemption is adopted.

```sql
ALTER TABLE staff_users
  ADD CONSTRAINT staff_users_active_requires_person
  CHECK (
    status <> 'active'
    OR person_id IS NOT NULL
    OR role = 'platform_admin'
  );
```

The `OR role = 'platform_admin'` clause follows migration `0011`'s own
role-conditioned `CHECK` pattern (`role = 'platform_admin' AND tenant_id
IS NULL) OR (role != 'platform_admin' AND tenant_id IS NOT NULL)`) rather
than inventing a new style for this table.

**(2) The remediation query's per-tenant framing structurally cannot see
`platform_admin` rows, and needed a second pass regardless of (1).**
`WHERE tenant_id = <t>` is never true against a NULL `tenant_id`, so no
number of per-tenant remediation runs examines a `platform_admin` row —
`identity-compliance`'s finding on this point stands on its own, whether
or not the exemption above is adopted, because remediation for a
*table-wide* constraint must actually cover the whole table.
**Remediation is therefore two passes, not one:**

```sql
-- Pass 1 — per tenant (unchanged from Fix Round 2's first pass):
SELECT id, email FROM staff_users
  WHERE tenant_id = <t> AND status = 'active' AND person_id IS NULL;

-- Pass 2 — NEW, second pass (identity-compliance, REQ-SEP-STAFF-1) —
-- platform-scoped staff, tenant_id IS NULL, invisible to pass 1:
SELECT id, email FROM staff_users
  WHERE tenant_id IS NULL AND status = 'active' AND person_id IS NULL;
```

**Pass 2 is kept even though the exemption in (1) means a surviving
`platform_admin` row it finds would not, in fact, fail the migration.**
This is stated plainly so it is not misread as belt-and-braces
busywork: `role = 'platform_admin'` and `tenant_id IS NULL` are the same
condition (migration `0011`'s own `CHECK` makes them equivalent), so
every row pass 2 would find is, by construction, a row the exemption
already covers — the exemption alone is sufficient for this migration to
succeed. Pass 2 is kept anyway for the reason stated as a "side effect"
below: several **other** triggers in the `0034`/`0044`/`0047` family
resolve the same `staff_users.person_id` column **without** this
exemption (they have no reason to carry a `SEP-1`-specific carve-out for
`platform_admin`), so knowing the true, whole-table state before this
migration ships — not just the state of the rows this one constraint
happens to gate — is cheap, honest, and consistent with `CLAUDE.md`'s
"verify actual implementation state" rule. It is not required for
*this* constraint's own correctness once (1) is adopted, and that
distinction is the whole reason it is spelled out rather than left
implicit.

This is a standard table `CHECK` constraint, evaluated by PostgreSQL on
every `INSERT` and `UPDATE` to `staff_users`, not a one-time read. It
cannot be silently bypassed by a code path the pre-deploy query's author
did not anticipate, and it turns "a tenant's promotions staff account
went active with no person link" from a possible-but-unverified state
into a rejected write, structurally, forever — while leaving
platform-wide staff exactly as unaffected by it as `SEP-1` already
renders them, per (1).

**The pre-deploy query does not disappear — its role changes.** Adding
this constraint to a table that already contains a violating,
non-exempt row fails the migration outright (the correct, fail-closed
behavior — it is cheaper to discover a violation at migration time than
to discover it was silently tolerated). The two-pass query above
therefore becomes the **remediation** step run immediately before the
migration that adds this constraint — clean the data, then make the
invariant permanent — rather than the control itself. This mirrors the
same distinction `SEP-1` draws everywhere else in this contract between
a check that fires once and one the database enforces continuously.

**Side effect, stated rather than claimed as a deliverable**: this
constraint hardens every existing `<table>_enforce_governance()` trigger
in the migration `0034`/`0044`/`0047` family that already depends on the
same precondition, not only `SEP-1`'s own triggers — because they all
resolve the same `staff_users.person_id` column. That is a welcome
side effect of closing `SEP-1`'s dependency correctly, not a new,
separately-scoped deliverable of this dispatch, and it is also exactly
why pass 2 of the remediation query is worth running even where the
`platform_admin` exemption makes it non-blocking for this constraint
specifically — those other triggers may not carry the same exemption,
and pass 2 is how their owners would find out before, not after,
deploy.

**Routed requirement — `REQ-SEP-STAFF-1`.** `security` does not own
`staff_users` or its migrations (CLAUDE.md, "no specialist redesigns
shared architecture unilaterally"). Routed to `identity-compliance`
(co-owner of the `staff_users.person_id` ↔ `Person` primitive per
`REQ-SEP-ID-1`, §W15.1.6) and whoever owns the `staff_users` migration
file in practice (`backend-platform`), flagged for `architect`'s
cross-domain sign-off: add `staff_users_active_requires_person` (with the
`platform_admin` exemption above) in the same migration that performs
**both passes** of the remediation query above, before any `SEP-1` or
`AFF-4E-1` trigger goes live in a non-development environment.
`security`'s own step-4 reading (1) is stated here as confirmed, but the
constraint's wording and the migration itself remain
`identity-compliance`/`architect`'s to land, per the same routing as Fix
Round 2's first pass.

**Tests, added to §B1.5/§W15.1.8's obligations:**

- An `UPDATE staff_users SET status = 'active' WHERE person_id IS NULL
  AND role <> 'platform_admin'` (attempted directly, bypassing every
  application code path) is rejected by the database with a constraint
  violation, not merely refused by application logic that could be
  routed around.
- **(New, second pass)** An `UPDATE staff_users SET status = 'active'
  WHERE person_id IS NULL AND role = 'platform_admin'` **succeeds** at
  the constraint level (the exemption does not over-block) — paired,
  positive-case test, so the exemption's correctness is asserted
  directly rather than inferred from the first test's absence of a
  false positive.
- **(New, second pass)** A `platform_admin` actor, `person_id` NULL,
  attempting a `SEP-1`-guarded operation on a real tenant's authorizing
  row is refused at **step 4** with reason code
  `SEP1_ACTOR_TENANT_MISMATCH` (§W15.1.11, second pass), distinct from
  `SEP1_ACTOR_UNRESOLVED`/`SEP1_ACTOR_IS_BENEFICIARY` — asserted
  specifically so a future relaxation of step 4 back to
  a bare `<>` (silently passing a `platform_admin` actor instead of
  refusing it) is caught here, not discovered as a live self-deal path
  later.

#### W15.1.11 Fix Round 2 (`RK-W15P2-6` part b / `RK-W15P2-7`) — the denial-audit record, redesigned per ADR 0031 §39

**The finding, and why it is correct.** §W15.1.8's original bullet
required "a `SEP-1` refusal writes an audit record with `Outcome:
denied`." `SEP-1` is specified as a `BEFORE INSERT/UPDATE` trigger that
`RAISE EXCEPTION`s (§W15.1.3). A `RAISE EXCEPTION` inside a `BEFORE`
trigger aborts the **entire enclosing transaction** — every write that
transaction attempted, including any audit-record insert the same
transaction tried to make, is rolled back with it. There is no
statement ordering, no savepoint discipline internal to that one
transaction, and no "write the audit row first" reordering that survives
this, because the abort is unconditional and total. The original
requirement was, as specified, impossible.

**The precedent that already solved this exact problem — ADR 0031 §39.**
Risk's `Evaluate` faces the identical shape: a `DENY`/`REVIEW` decision
returns *normally* (the transaction is still valid, so Bonus can write
its Progress/audit entry in that same transaction and commit); but a Go
`error` return means the transaction must abort, so **nothing written in
it survives**, and ADR 0031 §39 resolves this by requiring: *"An attempt
record, if the product wants one, must be written on a **separate**
transaction and must be unmistakably distinguishable from a policy
decline — an error means 'the platform could not decide,' not 'the
player's limits rejected this.'"*

**`SEP-1` maps onto the harder branch of that precedent, always — not
conditionally.** Unlike `Evaluate`, which has a real "return `DENY` and
let the caller commit a decline record in the same transaction" path,
`SEP-1` has no such path: it is a trigger, and a trigger's only way to
refuse an `INSERT`/`UPDATE` is to raise, which always aborts. Every
`SEP-1` refusal is therefore, structurally, the "error" row of ADR 0031
§39's table, never the "DENY, write in the same transaction" row. This
is stated because it would be a mistake to read `SEP-1` as sometimes
needing the separate-transaction pattern and sometimes not — it always
does.

**The fix — a separate-transaction denial-audit record, distinguished by
a reserved custom SQLSTATE, not by parsing a message string:**

1. Every `<table>_enforce_separation()` trigger raises using a single,
   shared, reserved custom SQLSTATE across the whole `SEP-1` trigger
   family — `'SP001'` throughout this document's pseudocode — never a
   generic PL/pgSQL default (`P0001`) and never a code shared with an
   ordinary `CHECK` constraint violation or with the four-eyes consume
   trigger's own raises (§B1.2 item 4, which should use its own distinct
   code, e.g. `'AC001'`, for the identical reason). A caller distinguishes
   "the database refused this because it is structurally a self-deal"
   from "the database refused this for an unrelated reason" by
   `pgconn.PgError.Code`, never by matching the `RAISE` message text,
   which §W15.1.3's own examples already show varies per refusal branch
   and is not a stable API.
2. The domain's Go call site — the code performing the `INSERT`/`UPDATE`
   that `SEP-1` may reject — catches the error, confirms `Code ==
   "SP001"`, and, **on a new transaction** (the original is already
   rolled back by Postgres; this is a fresh `Begin`, not a `Savepoint`
   inside the dead one), writes an `audit.Record` with:
   - `Outcome: denied`
   - `Action`: `<domain>_<object>.sep1_refused` (e.g.
     `bonus_grant.sep1_refused`, `bonus_bulk_job.sep1_refused`,
     `affiliate_commission_approval.sep1_refused`,
     `crm_engagement_campaign_activation.sep1_refused`) — a naming
     convention distinct from every existing `<domain>.<event>` action in
     §B1.4's catalogue, so a `SEP-1` refusal is never confusable, by
     `Action` alone, with an ordinary four-eyes policy decline (which
     keeps its own existing action name, e.g. `bonus_change_request
     .rejected`) or with an RBAC/RLS denial
   - `Metadata.reason_code` one of a small fixed set naming *which* step
     of §W15.1.3 (as amended by §W15.1.9, second pass) refused —
     `SEP1_ACTOR_UNRESOLVED`, `SEP1_ACTOR_IS_BENEFICIARY`,
     `SEP1_APPROVER_IS_BENEFICIARY`, `SEP1_EMPTY_BENEFICIARY_SET`,
     `SEP1_NULL_BENEFICIARY_PERSON`, `SEP1_CARDINALITY_MISMATCH`,
     `SEP1_TENANT_SCOPE_SELF_PROOF_FAILED`,
     `SEP1_ACTOR_TENANT_MISMATCH` (step 4, second pass — includes every
     `platform_admin` actor on a tenant-owned row, by the `IS DISTINCT
     FROM` correction above),
     `SEP1_SUBTREE_SCOPE_SELF_PROOF_FAILED` (Step 0b, second pass —
     `ancestor_closure` resolvers only), and
     `SEP1_RESOLVER_SHAPE_UNRECOGNIZED` (step 5's fail-closed `ELSE`,
     second pass) — never the resolved `person_id`s or beneficiary
     attributes themselves (§B1.4's PII rule applies to this record
     exactly as to every other)
   - the attempted operation's identity (tenant, target type/id,
     operation type) but **not** the payload the attempt tried to write,
     for the same reason a denied withdrawal's audit record does not
     replay the withdrawal amount's provenance beyond what is already
     safe to store
3. This is a **mandatory** obligation on every call site that performs a
   `SEP-1`-guarded write, not an optional enhancement — a refusal that
   writes nothing anywhere is exactly the "a control that fires silently
   leaves no evidence it fired" failure §B1.4 already names, now
   correctly engineered to actually be achievable.

**Honest residual limitation, stated rather than hidden.** The
separate-transaction write happens *after* the aborting transaction has
already unwound, in the same application process, on the same request.
A process crash or connection loss in the narrow window between the
abort and the separate audit write is a real, if narrow, gap: the
`SEP-1` refusal itself is not lost (the `INSERT`/`UPDATE` did not
happen — the ledger/state invariant holds), but the **evidence that it
was attempted** could be. This is the same residual risk ADR 0031 §39's
own pattern accepts implicitly for Risk's `error` branch, and this
document does not claim to close it — it is recorded as a known,
accepted gap, not a solved one. A future outbox-pattern or
write-ahead-log-based delivery would close it; building one is out of
this dispatch's scope and is not required to close `RK-W15P2-7`, which
concerns the record's *specification*, not its delivery guarantee.

**Test, replacing and extending §W15.1.8's original bullet (already
restated above in place):**

- A `SEP-1` refusal produces **zero** rows in the authorizing table (the
  transaction fully rolled back) and **exactly one** separate-transaction
  audit record with `Outcome: denied`, an `Action` ending in
  `.sep1_refused`, and a `Metadata.reason_code` matching the specific
  branch that fired.
- The `SEP-1`-refusal audit record's `Action` is asserted to differ from
  the ordinary four-eyes decline `Action` for the same table, so a test
  that only checks "an audit record with `Outcome: denied` exists" —
  which would pass even if the two were conflated — is insufficient and
  must not be the only assertion `qa` accepts.
- The custom SQLSTATE (`'SP001'`) is asserted directly on the caught
  Postgres error, not inferred from message text, as the mechanism the
  application-layer catch relies on.

#### W15.1.12 Fix Round 2 — the Bonus adoption contract (`REQ-SEP-BONUS-1` … `4`), consolidated and made unambiguous

This subsection is the single, self-contained checklist `bonus-engine`
implements against in its own dispatch this round. It restates
`REQ-SEP-BONUS-1`–`3` (§W15.1.7) in full rather than by cross-reference,
and formalizes the newly-discovered `REQ-SEP-BONUS-4`. Nothing here
weakens or reopens the general `SEP-1` mechanism (§W15.1.3, as amended by
§W15.1.9–§W15.1.11); this subsection only fixes the **enforcement
points, resolvers and permissions** Bonus must wire it into.

| Req | Enforcement point(s) | Beneficiary resolver `B(O)` | Permission gated | Four-eyes independent of `SEP-1`? |
|---|---|---|---|---|
| **`REQ-SEP-BONUS-1`** | Grant issuance, Grant activation, direct bonus Adjustment, staff-forced conversion/manual release override, `BulkGrantJob` execution | Issuance/activation/adjustment/forced-conversion: the target Grant's/target wallet's `player_account_id → player_accounts.person_id` (scalar, `expected_count = 1`). `BulkGrantJob`: the **set** of persons behind the pinned, materialized recipient set (`expected_count` = the pinned row count, §W15.1.9) | `bonus_grant:issue`, `bonus_adjustment:write`, `bonus_bulk:execute` (existing, §B1.1) | Yes — §B1.2 items 1/2/3/4, unchanged. `SEP-1` is additional, per §W15.4.1 |
| **`REQ-SEP-BONUS-2`** | `BonusSuggestion` review (approve/reject) and activation | The persons behind the resolved `proposed_player_population`, same pinned-set discipline as `BulkGrantJob` | `bonus_suggestion:review` (existing, §W15.4.3); activation consumes the underlying Grant/BulkJob permission, per §W15.4.3 item 3 | Activation's own existing four-eyes (unchanged); review itself is not separately four-eyes-gated (§W15.4.3) |
| **`REQ-SEP-BONUS-3`** | The household/linked-account **detection** path of §W15.1.5 | n/a — this is a "never a block" requirement, not a resolver | n/a | n/a |
| **`REQ-SEP-BONUS-4`** (**new, this round**) | `HeldDispositionRecord` resolution (doc 10 N1.4.1 item 5c/N1.9): `ACTION_REFORFEIT`, `ACTION_ROUTE_TO_CASH`, and the manual sub-choice under `ACTION_HOLD_FOR_REVIEW`. **Not** the `HeldDispositionRecord`'s *creation* (doc 10's own row 7/12 classify creation as `TECHNICAL` — "recording-and-parking a fact," never an authorizing write) | The Grant's `player_account_id → player_accounts.person_id` — identical shape to `REQ-SEP-BONUS-1`'s adjustment/forced-conversion resolver, because a `HeldDispositionRecord` resolution moves value onto or off of exactly that Grant's player | **New, dedicated**: `bonus_held_disposition:resolve` (see below — must not be folded into `bonus_adjustment:write` or `bonus_bulk:execute`) | **Yes — the same shape as `bonus_adjustment:write` (§B1.2 item 3): a tenant-configurable threshold whose fail-closed default is 0.** Doc 10 §N1.8.1 rows 8–10 classify every resolution action as `POLICY-DEPENDENT`, requiring a human G-2 answer, which makes it at least as material as an ordinary adjustment — no more, no less. See the certification correction below: this cell previously read "threshold 0 (always)," which read as an unconditional, non-configurable floor (the `bonus_bulk:execute` shape); that was this document's own drafting ambiguity, not a considered departure from CLAUDE.md's general rule, and is corrected here |

**Why `REQ-SEP-BONUS-4` needs a dedicated permission, stated as an
argument, not an assertion.** The human directive's framing — "must
become a `SEP-1` enforcement point with its own named permission (not
folded into `bonus_grant:adjust` or `bonus_bulk:execute`)" — is correct
for the same reason §B1.1 already split `bonus_config:manage` into six
narrower authorities rather than leaving it bundled: a
`HeldDispositionRecord` resolution is a distinct economic act (it
resolves a G-2-policy-dependent, already-deferred value disposition,
not an ordinary balance correction and not a bulk operation) with its
own volume, its own risk profile, and its own reporting need. Folding it
into `bonus_adjustment:write` would let a tenant's existing
adjustment-authorized staff resolve deferred dispositions **without any
role-wiring decision ever being made about it** — the exact "configure
it instead of adjusting it" bypass shape §B1.1 hard constraint 3 already
exists to close, recurring one layer over. A dedicated permission also
lets `qa`/audit distinguish held-disposition-resolution volume from
ordinary adjustment volume without inference, which the mixed-purpose
alternative cannot do.

**Phase 2 independent-certification correction (`security`, self-disclosed
— not a re-review of the whole document, a fix to this one cell).**
`bonus-engine`'s N1.12 correctly flagged, without resolving, an apparent
conflict between this table's four-eyes cell and doc 34 §3.1's row for
`bonus_held_disposition_resolution` ("four-eyes above `CLAUDE.md`'s
threshold — same shape as `manual_balance_adjustment`"). Having reviewed
both texts side by side: **doc 34 §3.1 was right; this document's own
"(always)" wording was the defect**, and is corrected above rather than
left standing for a second round. The reasoning: `bonus_held_disposition:
resolve` gates a **scalar, single-Grant** action (`expected_count = 1`,
identical shape to an ordinary adjustment or forced conversion) — it is
not a blast-radius operation the way `BulkGrantJob` execution is. The
argument this document makes above for a **dedicated permission**
(distinct volume, distinct risk profile, distinct reporting need, closing
the "configure it instead of adjusting it" bypass one layer over) is
sound and unaffected by this correction — but it is an argument for
*separating the authority*, not for making its *threshold* unconditional
in a way ordinary adjustments are not. CLAUDE.md's own baseline is "four-
eyes approval above a **configurable** threshold" for manual balance
adjustments generally; §B1.2 item 3 already applies that verbatim to
`bonus_adjustment:write` (default 0, tenant may raise it). Nothing about
a `HeldDispositionRecord` resolution's economics distinguishes it from
that baseline the way `bonus_bulk:execute`'s blast-radius reasoning
(§B1.2 item 2 — "a per-player threshold applied to a bulk job is not a
control") distinguishes bulk execution. **Binding, corrected statement**:
`bonus_held_disposition:resolve`'s four-eyes threshold is
tenant-configurable, defaulting (fail-closed, absent a policy row) to 0 —
the `bonus_adjustment:write` shape — not a hardcoded, non-configurable
zero. `bonus-engine`'s N1.12 deferred to this document's "stricter,
unconditional reading" for its own text; that deference is now moot,
since the stricter reading was the error being deferred to. **Routed**:
`bonus-engine` should update N1.12's own four-eyes cell to match this
correction (drop "threshold 0, always" / "unconditional regardless of
amount," state "configurable, default 0, per §B1.2 item 3's shape")
rather than carry the now-corrected inconsistency forward silently; this
is a citation-alignment fix on Bonus's side, not a reopening of anything
substantive `bonus-engine` designed. Doc 34 §3.1 needs no change — it was
already correct.

**Wiring, extending §B1.1's tables (routed for `architect`/
`bonus-engine`'s sign-off, per the same flag §B1.1 already carries for
its own permission proposals):**

- `bonus_held_disposition:resolve` is granted to `RoleBonusOperations`
  only — the same role that holds `bonus_adjustment:write` and
  `bonus_grant:cancel` — never to `RolePromotionsManager`,
  `RoleTenantAdmin`, `RoleFinance`, or `RolePlatformAdmin`, for the
  identical reasons §B1.1 already gives for those roles holding no other
  bonus write authority.
- §B1.1 hard constraint 3 ("No role may hold both `bonus_offer:manage`
  and `bonus_adjustment:write`") is extended verbatim to
  `bonus_held_disposition:resolve`: no role may hold both
  `bonus_offer:manage` and `bonus_held_disposition:resolve`.
- Audit (extending §B1.4's catalogue): `bonus_held_disposition.resolved`
  — actor staff; `TargetType` a placeholder pending `bonus-engine`'s
  Wave-1 domain model per §B1.7 dependency 1 (the `HeldDispositionRecord`
  table/type name itself); reason code **always**; before/after the
  `status` transition (`held → resolved_reforfeit` /
  `resolved_route_to_cash`), the amount, `change_request_id`, approver
  ids, `threshold_at_decision` — the same shape as
  `bonus_adjustment.applied` (§B1.4).
- `SEP-1` applies at the resolution row exactly as specified in
  §W15.1.3 (as amended by §W15.1.9–§W15.1.11): step 0 tenant-scope
  self-proof, actor resolution, cardinality-asserted resolver, the
  unconditional comparison, and a separate-transaction denial-audit
  record on refusal.

**Fail-closed statement (per the human directive's instruction not to
invent identity data that doesn't exist).** Where the `HeldDispositionRecord`'s
target Grant's `player_accounts.person_id` cannot be resolved — an
unlinked or malformed player account, a cross-tenant reference, any
resolution failure — the resolution is **refused**, not defaulted to
"resolve without a `SEP-1` check" and not defaulted to "resolve as if no
conflict exists." This is the same fail-closed posture §W15.2.4 already
states for the affiliate side, restated here so Bonus's adoption does
not silently diverge from it.

**Test additions to §B1.5/§W15.1.8:**

- A staff actor whose `person_id` equals the target Grant's player's
  `person_id` attempting `ACTION_REFORFEIT` or `ACTION_ROUTE_TO_CASH` is
  refused by `SEP-1`, at a single Grant (amount 1, audience size 1).
- No role in `permission.go`'s map holds `bonus_held_disposition:resolve`
  together with `bonus_offer:manage`.
- A `HeldDispositionRecord` **creation** event (doc 10 row 7/12) requires
  no permission beyond the existing settlement/capture path and is
  **not** `SEP-1`-gated — asserted explicitly, so the TECHNICAL/
  POLICY-DEPENDENT boundary doc 10 already draws is not blurred by an
  over-broad implementation that gates the wrong event.
- `bonus_held_disposition.resolved` is audited with the full
  before/after shape above; a resolution attempted with an unresolvable
  target player is refused, not defaulted.

### W15.2 Affiliate approval independence (closes SEC-W15-01; decides DEP-AFF-1)

#### W15.2.1 Vocabulary — the concepts that must stay distinct

The SEC-W15-01 failure is a vocabulary failure before it is a control
failure: doc 32 §6.5 says "the approver must be a distinct, resolved,
active person from the requester", which is true, enforceable, and does
not close the hole, because *two different people can be one commercial
interest*. The terms below are therefore fixed for every affiliate
control.

| Term | Definition | What it does **not** imply |
|---|---|---|
| **Actor identity** | The authenticated principal performing the operation: a `staff_users.id`. | Nothing about who controls it, or on whose behalf. |
| **Person** | The `persons` row the actor resolves to. The platform's only *deterministic* unit of human identity. | Nothing about employment, entity, or interest. |
| **Legal / organizational authority** | The legal entity (company, sole trader) on whose behalf an actor acts. **Not modeled anywhere in this platform today.** | It must never be *inferred* from role, node position, or email domain. Inference here is how a control silently becomes decorative. |
| **Affiliate entity** | The counterparty to the `AffiliateAgreement` — a legal entity. One entity may hold **many** nodes and **many** accounts. | Not the same as a node, and not the same as an account. |
| **Affiliate node** | A position in `agentnetwork`'s tree (doc 32 §3). A *structural* position. | Two distinct nodes are **not** two distinct entities; sub-affiliates may all sit under one entity. |
| **Affiliate account** | A `staff_users` row with an affiliate role, subtree-scoped. | Two distinct accounts are **not** two distinct interests, and two distinct `person_id`s are **not** two distinct interests either. |
| **Approver** | The principal recording an `approve` decision on an affiliate-financial object. | Holding the approval permission does not make a principal independent of the beneficiary. |
| **Subject** | The object acted upon: the accrual, the attribution, the settlement instruction, the agreement version. | Not the same as the beneficiary — a re-attribution's subject is an attribution row; its beneficiaries are two nodes' entities. |
| **Beneficiary** | Every party to whom value accrues if the operation succeeds. For a commission decision this is the **reflexive ancestor closure** of the commission owner — the node **itself**, together with its ancestor chain — under any override agreement (doc 32 §6.4). **Ancestors alone is wrong** (§W15.2.7, `NEW-6`): it excludes the node's own beneficiaries and, for a flat/root node with no ancestors, resolves empty — which `SEP-1` treats as a refusal, deadlocking the platform's own first-slice affiliate topology. | Not limited to the node on the accrual, **and** not limited to its ancestors either. A parent's override interest makes the parent a beneficiary; the node's own people are beneficiaries of the node's own accrual. |
| **Commission owner** | The node the accrual names — the direct claimant. | Not the only beneficiary. |

**The binding consequence of this vocabulary**: two accounts controlled by
the same underlying affiliate entity must **not** automatically satisfy
independence — and the platform today has **no capability whatsoever** to
determine whether two accounts share an entity. That is why the fix cannot
be "the approver must be a different affiliate account", and why building
an entity-resolution capability is not a precondition for closing the P0.

#### W15.2.2 DEP-AFF-1 — formal decision

> **DECISION (`security`, owner of DEP-AFF-1): CONDITIONAL ACCEPT.**
> Affiliate users may be `identity.StaffUser`s carrying affiliate-specific,
> subtree-scoped roles, rather than a new `auth.PrincipalType`, **subject
> to the four binding conditions below**. Without all four, the reuse is
> refused and a distinct principal type is required instead.

Rationale for accepting rather than forking the principal type: a new
`PrincipalType` would fork session issuance (`internal/auth/jwt.go`),
audit actor typing (`internal/audit`), the `sessions` table's
`principal_type` semantics, every RLS predicate, and every permission
check — a large cross-cutting change whose entire security benefit is
reproducible by one positively-stored classification column plus the
conditions below. Doc 26's cashier precedent is sound *structurally*.
`architect` is right that the structure is the same; `architect` is also
right (doc 32 §3.2) to flag that the security question is different,
because a cashier is our staff and an affiliate is an external commercial
counterparty holding a credential in our staff principal space. The
conditions are what make those two facts compatible.

**AFF-C1 — positive principal classification, never a blocklist.**
`staff_users` gains a `principal_class` column (`internal` |
`external_affiliate`, extensible), `NOT NULL`, **with no permissive
default**; every existing row is backfilled explicitly as `internal` in
the same migration that mints the first affiliate role. Every control that
means "our own staff" must test `principal_class = 'internal'`
**positively**. It must never be expressed as `role NOT IN (<affiliate
roles>)`.

*Concrete failure scenario for the blocklist form*: migration `0041` shows
exactly how a role is added in this codebase — by editing the `role` CHECK
constraint on `staff_users` in a later migration. A blocklist is a list
someone must remember to update. The next affiliate-side role added by a
migration that doesn't touch the blocklist is silently *internal*, and
silently able to approve its own side's commission. The positive form
fails **closed** under precisely the same omission: an unclassified new
role approves nothing.

**AFF-C2 — subtree scoping must be structural before any affiliate-facing
surface ships.** The `app.hierarchy_node_id` / `WithNodeScope` RLS
dimension (DEP-AFF-5, already P1 from doc 26 §8) must exist and be
enforced at the database, not as an application query filter. Doc 32 §10
asserts this is structural; this condition makes it a precondition rather
than an assumption. Until it lands, no affiliate principal exists in any
non-development environment.

**AFF-C3 — affiliate roles hold a disjoint permission set.** No
affiliate-class principal holds any permission that exists today.
Specifically never: `staff:manage`, `audit:read`, `verification:read`,
any `rg_*`, any `risk_*`, any `withdrawal:*`, any `bonus_*`, any
`crm_*`. Mechanically checkable: the intersection of every affiliate
role's permission set with every non-affiliate role's permission set is
empty.

**AFF-C4 — the `AFF-4E-1` rule below (§W15.2.3).** This is the condition
that closes SEC-W15-01.

*Recommendation, not a condition* (recorded so it is not later read as an
omission): affiliate accounts are external-party credentials on the staff
authentication path and warrant mandatory MFA, a separate lockout/rate
policy, and separate session TTLs from internal staff. `security` does not
make this binding at design time because it is an operational control
with no design dependency — but it should be decided before the first
non-development affiliate account exists.

#### W15.2.3 `AFF-4E-1` — the binding rule (closes SEC-W15-01)

> **Every approval decision on any affiliate-financial object must be
> recorded by a principal whose `principal_class = 'internal'`.** No
> principal of any other class may record an `approve` on a
> `CommissionApproval`, a `CommissionSettlementInstruction`, a
> re-attribution (doc 32 AI-6), an `AffiliateAgreementVersion` or
> `CommissionRuleVersion` activation, or any node/agreement status change
> that alters commercial terms. **Unconditional — no threshold, no
> delegation, no emergency override, no service-identity carve-out.**

**Is this sufficient on its own to close the P0? Yes — and here is the
argument, rather than an assertion.** SEC-W15-01's failure scenario was:
two affiliate-side accounts, distinct `person_id`s, both controlled by one
commercial interest, satisfying `approver_person_id <> requester_person_id`
and thereby satisfying four-eyes while providing no independence at all.
If no affiliate-class account can ever record an approval on affiliate
money, then:

- The second colluding account has **no approval capability to
  contribute**. Collusion between two affiliate accounts becomes moot —
  not detected, not mitigated: structurally irrelevant, because neither of
  them can be the approver.
- The residual threat is an affiliate colluding with an **internal**
  employee. That is a materially different and smaller threat: it requires
  suborning an employee; it leaves an audit trail naming an internal
  person with an internal employment relationship and internal
  consequences; and it is the threat that four-eyes, `SEP-1`, and the
  attestation layer of §W15.2.5 are collectively for.
- It requires **no new identity capability at all** — no affiliate-entity
  resolution, no beneficial-ownership graph, no KYC of affiliate
  principals, no biometrics. It is one positively-stored class comparison
  in the same trigger family as everything else in this contract. That is
  why it is the correct fix *now*, and why an entity-resolution project is
  not a precondition for closing a P0.

**What `AFF-4E-1` does not close, stated plainly:**

1. An **internal** staff member who beneficially owns an affiliate node
   approving their own node's commission. `AFF-4E-1` sees a compliant
   internal approver. This is the staff-as-affiliate gap named in
   `security`'s Phase 2 report as an unowned finding, and §W15.2.5 is a
   **separate, additional** control for it — not a refinement of this one.
2. Two internal staff colluding. Nothing short of role segregation,
   detection, and reconciliation addresses that; it is not affiliate-
   specific and is not claimed here.
3. Anything about whether the commission *amount* is right. `AFF-4E-1` is
   an independence control, not a correctness control; doc 32 AI-7's
   ledger-derived measure and `ledger-finance`'s DEP-AFF-4 own that.

**Two supporting requirements that travel with `AFF-4E-1`:**

- **`SEP-1` applies at the same point**, with the beneficiary set of
  §W15.1.2 (the node's **reflexive ancestor closure**'s affiliate-account
  persons **plus** its declared beneficial-interest persons — §W15.2.7;
  not ancestors alone). `AFF-4E-1` handles "which class may approve";
  `SEP-1` handles "which person may not".
- **Approvals must be *consumed*, not referenced.** This is SEC-W15-12:
  `CommissionSettlementInstruction.approval_refs[]` as specified is *data
  carried alongside* the instruction, which means the instruction is
  self-attesting. It must instead go through a consume function in the
  §B1.2 item 4 shape — select the matching `pending` request `FOR UPDATE`,
  require the approval rows, mark it `applied` in the same statement — so
  that one approval authorizes one settlement, once. Routed as
  **REQ-AFF-CONSUME-1** to `architect`. (Accrual-level idempotency —
  `ledger-finance`'s LF-6 — is a separate finding and is not closed here.)

#### W15.2.4 Fail-closed defaults where the authority information does not exist

Every one of these refuses; none of them approves-by-default, and none of
them warns-and-continues:

| Condition | Result |
|---|---|
| Approver's `principal_class` is NULL, absent, or an unrecognized value | **Refuse the approval.** |
| The beneficiary set cannot be fully resolved (missing attestation record, incomplete reflexive ancestor closure, unresolvable agreement version, unresolvable node) | **Refuse the approval.** |
| The node's beneficial-ownership attestation is **missing** (`undeclared`) | **Refuse** every approval on that node's objects. |
| The attestation exists but is **stale** past its re-attestation period | **Refuse.** Stated explicitly so that nobody implements "stale ⇒ warn". |
| An affiliate-class principal attempts an approval | **Refuse** (`AFF-4E-1`). |

The commercial consequence is that an accrual stays `pending_approval`
until the missing information exists. That is the correct trade and it is
worth stating why: an **unpaid** affiliate is a commercial problem with a
queue, an owner, and a remedy. An **incorrectly paid** one is an
unrecoverable outflow to an external party outside our jurisdiction and
usually outside our recovery options.

#### W15.2.5 Beneficial-ownership attestation — a separate control for staff-as-affiliate

Specification level only; the physical home (node vs. agreement) is
`architect`'s choice.

**Shape.** An append-only `affiliate_beneficial_interest_attestations`
table — never a mutable column set on the node — with the node carrying a
pointer to its current row:

| Field | Rule |
|---|---|
| `node_id`, `tenant_id` | scope; composite-FK'd per §B1.3's rule |
| `declaration` | `none_internal` \| `internal_interest_declared` \| `undeclared`. **`undeclared` is the default state of a new node**, and it refuses approvals (§W15.2.4) |
| `declared_interest_person_ids[]` | resolved `persons` references for every internal person declared to hold an interest |
| `attested_by_principal_id`, `attested_at`, `attestation_reason_code` | who declared, when, why |
| `attestation_period_days` | tenant configuration |
| `next_attestation_due_at` | derived; past it, the node is `attestation_stale` |
| supersedes / superseded_by | a change is a **new row**, never an edit (`withdrawal_policies_deny_update` precedent) |

**Effect — and this is the whole point of the control:**
`declared_interest_person_ids` **join the node's `SEP-1` beneficiary set**.
A declared internal owner is therefore structurally unable to act on, or
approve, that node's money. Declaring is not a confession that costs the
declarer anything discretionary; it is the mechanism that protects them
from ever being the single point of failure on their own node.

**Who attests.** An internal affiliate-relationship owner, with four-eyes
at threshold **0** (always). An attestation is the *input to a control*,
so an unwitnessed self-serving `none_internal` declaration defeats the
control entirely. And by `SEP-1` applied to itself: the attester may not
be a declared interest holder on the node they are attesting.

**Audit.** `affiliate_node.beneficial_interest_attested`, reason code
always, before/after (superseded row → new row), per §B1.4's shape.

**Honest limitation.** An attestation is a *declaration*. It catches the
honest-but-conflicted case, and it creates a disciplinary and contractual
hook for the dishonest one. **It does not detect an undisclosed
interest**, and nothing in this section claims it does. Detecting
undisclosed beneficial ownership requires payout-instrument and identity
correlation that belongs to `identity-compliance` and `risk` (doc 32
OI-AFF-4's routing), not to Affiliate and not to `security`. The one
detection signal that *is* cheaply available and should be wired: a
commission settlement whose payout destination or instrument correlates
with a known internal staff person's or a player account's instrument —
routed to the same detection path as §W15.1.5, as a **signal**, never as
an automatic block, for the identical reason.

#### W15.2.6 Adversarial tests required for this control

Design-level; consolidated here specifically for affiliate approval
independence.

1. **Same user** — the requester records their own `approve`: refused
   (`UNIQUE (request_id, approver_principal_id)` plus the trigger).
2. **Same person, two accounts** — two `staff_users` rows sharing one
   `person_id`: refused.
3. **Same affiliate account** approving an accrual on its own node:
   refused by `AFF-4E-1` (class), *and independently* by `SEP-1`
   (beneficiary). Both must be asserted separately — a test that only
   proves "refused" cannot tell which control is carrying the weight, and
   one of them silently going inert must be detectable.
4. **Two distinct affiliate accounts, distinct persons, colluding** — the
   literal SEC-W15-01 scenario: refused by `AFF-4E-1`.
5. **Parent approving a sub-affiliate's accrual** where an override
   agreement makes the parent a beneficiary: refused by `SEP-1`'s
   reflexive-ancestor-closure resolver.
6. **Sub-affiliate approving its parent's accrual**: refused by
   `AFF-4E-1`.
7. **Same beneficial authority where represented** — an internal staff
   member listed in `declared_interest_person_ids` for the benefiting node
   approving its commission: refused by `SEP-1`.
8. **Undeclared node** (`declaration = 'undeclared'`): every approval
   refused, and the accrual remains `pending_approval`.
9. **Stale attestation** past `next_attestation_due_at`: refused, not
   warned.
10. **Two unrelated internal authorized approvers**, neither a declared
    interest holder, neither in the reflexive ancestor closure: **succeeds.**
    (The positive case that proves the control set is not inert —
    `SEP-1-H1`.)
11. **Replay** — the same approved settlement instruction submitted twice:
    posts once, by database constraint, not by application check.
12. **Concurrent approvals** — two approvers racing to be the second
    approval: exactly one settlement is consumed; the request cannot reach
    `applied` twice.
13. **Approve-then-reject** and **reject-then-approve**: a `reject` is not
    overridable by a later `approve` (§B1.2 item 4).
14. **Payload mutation after approval** — the accrual set or amount
    changed between approval and consumption: refused by the payload
    match.
15. **Unclassified principal** — `principal_class` NULL or an unknown
    value: refused (the `AFF-C1` regression guard; this is the test that
    catches a future role added without classification).
16. **Cross-tenant** — an internal approver from tenant B approving tenant
    A's accrual: refused.
17. **Cross-subtree read** — an affiliate principal reading another
    subtree's accrual/attribution returns zero rows under RLS, not a
    filtered application response.
18. **Same node, not an ancestor** — an internal staff member with a
    declared beneficial interest in the **subject node itself** (the
    commission owner, not a parent) approving that node's own commission:
    refused by `SEP-1`. (New, Fix Round 2 — `code-reviewer`'s `NEW-6`; a
    strict ancestors-only resolver does **not** catch this, because the
    subject node is not its own ancestor.)
19. **Flat/root node, no ancestors** — the platform's own first-slice
    affiliate topology (no sub-affiliates): an unrelated, unconflicted
    internal approver approving that node's commission **succeeds**. (New,
    Fix Round 2 — proves the reflexive fix also closes the deadlock branch
    of `NEW-6`: an ancestors-only resolver would return an empty `B(O)`
    for a node with no ancestors, and `SEP-1` treats empty as a refusal,
    so *every* commission in the recommended first slice would have been
    permanently unapprovable.)

#### W15.2.7 Fix Round 2 (`NEW-6`) — the reflexive ancestor closure correction

**The finding.** `code-reviewer` found that doc 32 §6.5.2's corrected
resolver — itself a fix for the original SEC-W15-01 collusion finding —
defines `B(O)` as the commission owner node's "ancestor chain," which on
its plain reading **excludes the node itself**. This document's own
§W15.1.2 table and §W15.2.1 vocabulary entry (both `security`'s text, not
doc 32's) used the identical non-reflexive phrasing and are corrected in
place, above, rather than left standing while this subsection alone
states the fix — CLAUDE.md's "no specialist redesigns shared architecture
unilaterally" means `security` does not edit doc 32 itself, but it does
not excuse `security`'s own document from repeating doc 32's bug.

**The two failure branches, both real, both closed by the same one-word
fix.**

1. **Fail-open.** §W15.2.5 specifies that
   `declared_interest_person_ids` "join the node's `SEP-1` beneficiary
   set" — but if `B(O)` is computed as ancestors-only, an internal staff
   member who declared a beneficial interest in the paying node **itself**
   is never in `B(O)` at all, because the node is not its own ancestor.
   That staff member could approve their own node's commission, with a
   properly-filed, properly-witnessed attestation on record, and `SEP-1`
   would not fire. This directly defeats §W15.2.5's stated purpose — "a
   declared internal owner is therefore structurally unable to act on, or
   approve, that node's money" — which is only true if `B(O)` includes the
   node itself.
2. **Deadlock.** `SEP-1` treats an empty `B(O)` as a refusal (§W15.1.3
   step 5, unchanged by this fix) — correctly, per §W15.1.1's "an
   operation whose beneficiary set resolves empty is refused" and its own
   stated reasoning ("this operation benefits nobody identifiable" is as
   ineligible as benefiting the actor). A flat/root affiliate node with no
   sub-affiliates — which doc 32 and the platform's own commercial plan
   name as the **first slice**, deliberately the simplest topology — has
   an empty ancestor set by construction. Under an ancestors-only reading,
   `B(O)` for every commission on that node is empty, so **every**
   commission approval on the platform's own recommended launch topology
   would be refused, forever, by a control whose entire purpose is to
   distinguish a legitimate approver from an illegitimate one — not to
   forbid approval altogether.

**The fix.** `B(O)` for `CommissionApproval`, `CommissionSettlementInstruction`,
re-attribution, and agreement/rule-version activation is the **reflexive**
ancestor closure:

> `B(O) = {commission owner node} ∪ ancestors(commission owner node)`,
> expanded to that closure's affiliate-account persons **plus** its
> declared beneficial-interest persons — never ancestors alone.

This is a one-word correction ("ancestor chain" → "reflexive ancestor
closure") with two consequences that were previously both wrong in
opposite directions: `B(O)` is now **never empty** for any node (a node
is trivially a member of its own reflexive closure), which closes the
deadlock; and the node's own affiliate-account persons and declared
interest-holders are now always tested, which closes the fail-open.
Nothing else about `SEP-1`'s mechanism (§W15.1.3, as amended by
§W15.1.9–§W15.1.11) changes — this is a correction to one domain's
supplied *resolver definition* (§W15.1.2 item 1), not to the shared
template.

**What this does not change.** `AFF-4E-1` (§W15.2.3) is untouched — it
already refuses any affiliate-class approver regardless of beneficiary
set, so it was never affected by the ancestors-only bug. The reflexive
fix is purely a `SEP-1`-side correction to the beneficiary-set
*definition*, addressing the residual "internal staff member as
beneficiary" gap `AFF-4E-1` explicitly does not claim to close (§W15.2.3,
"What `AFF-4E-1` does not close, stated plainly," item 1).

**Routed.** The actual edit to doc 32 §6.5.2's own prose (and to AI-6's
test list there) is `architect`'s, in a separate dispatch this round —
`security` states the corrected rule here as the authoritative contract
text so that `architect`'s doc 32 edit has an unambiguous target to match,
per the human directive's instruction that this document's own
restatement must not perpetuate the non-reflexive wording while doc 32
is fixed separately.

#### W15.2.8 SEC-W15-01 adversarial test matrix, closure honesty (human directive, Fix Round 2)

The human directive requires an explicit adversarial test matrix for
SEC-W15-01 specifically, naming eight categories, and requires this
document to state plainly which of them the fixed design (`AFF-4E-1` +
`SEP-1` with the §W15.2.7 reflexive correction) actually closes versus
what remains residual. §W15.2.6 already contains 19 numbered cases (17
original, 2 added by §W15.2.7); this table maps the directive's eight
named categories onto them rather than re-deriving a second list, and is
the honesty statement the directive requires.

| Directive category | Mapped test(s) in §W15.2.6 | Closed by this design? | Residual, if any |
|---|---|---|---|
| **Same person** | 1, 2 | **Yes.** `UNIQUE (request_id, approver_principal_id)` plus the person-comparison trigger, unconditionally (the migration-`0034`-line-117 inversion is required to be absent, §W15.1.3). | None known. |
| **Same affiliate** | 3, 4 | **Yes**, by `AFF-4E-1` alone — no affiliate-class principal may ever record an approval, so two affiliate accounts on the same or different nodes cannot contribute an approval regardless of collusion. | None for the P0 scenario itself. `AFF-4E-1` does not, and does not claim to, detect that two affiliate *entities* are the same commercial party where distinct nodes and accounts are used to route around it (§W15.2.1's stated platform-wide identity-capability gap) — but that gap is moot here because neither can approve at all. |
| **Same parent affiliate** | 5, 6 (and 18/19 for the reflexive fix) | **Yes**, by `SEP-1`'s reflexive ancestor closure — a parent benefiting from a child's override is now always in `B(O)`, and the node's own case (18) and the empty-ancestor case (19) are both covered. | The *deadlock* branch of `NEW-6` was itself introduced by a previous fix and closed only in this round; no further residual is claimed here beyond the honest limits of §W15.2.5 below. |
| **Same economic owner where represented** | 7 | **Yes, where an attestation exists.** An internal staff member with a `declared_interest_person_ids` entry for the node is in `B(O)` and is refused. | **Residual, stated plainly (§W15.2.5's own limitation, restated here because the directive asks for honesty specifically):** an *undisclosed* beneficial interest is not detected — an internal staff member who is a true economic owner but never attested to it is not in `B(O)` and is not refused by `SEP-1`. §W15.2.4's `undeclared` default at least forces the node into a refused state until *someone* attests, but a dishonest or negligent attester (`none_internal` when the truth is otherwise) defeats the control. This is not solved by this fix and is not claimed to be — detecting an undisclosed interest requires payout-instrument/identity correlation routed to `identity-compliance`/`risk` (§W15.2.5), not an affiliate-local or security-local heuristic. |
| **Unrelated authorized approvers** | 10 (renumbered from the original list; also 19) | **Yes** — the positive case proving the control set is not universally inert (`SEP-1-H1`). | None known, subject to the resolver actually running (§W15.1.9's cardinality assertion is what makes "unrelated" provably unrelated rather than accidentally-truncated-to-look-unrelated). |
| **Replay** | 11 | **Yes** — by database constraint (the consume function's state transition, §B1.2 item 4 shape, `REQ-AFF-CONSUME-1`), not by an application check that could be skipped. | None known. |
| **Concurrency** | 12 | **Yes** — `SELECT … FOR UPDATE` serializes the consume; two racing approvals cannot both reach `applied`. | None known beyond the general residual audit-record-loss-on-crash limitation stated in §W15.1.11, which is about evidence of a denial, not about the operation's own correctness. |
| **Missing authority** | 15 (unclassified `principal_class`), plus every row of §W15.2.4's fail-closed table | **Yes** — every listed missing-authority condition (unclassified principal, unresolvable ancestor closure, undeclared attestation, stale attestation) refuses; none defaults to approve. | None known for the *listed* conditions. A condition this document has not enumerated could in principle default open if a future implementation adds a new authority-bearing field without adding its own fail-closed row — this is a general hazard of any enumerated fail-closed table, not specific to this control, and is why §W15.2.4's table is phrased as exhaustive-as-of-today rather than exhaustive-forever. |
| **Partial identity resolution** | new test under §W15.1.9 ("a resolver whose join is made to cross a row outside the acting connection's RLS scope") | **Yes, at the mechanism level** — the cardinality assertion (§W15.1.9) refuses whenever the resolved count differs from the expected count, which is exactly the shape a partial/truncated resolution takes. | **Residual, stated honestly:** the cardinality assertion catches *undercounting* (rows silently dropped). It does not, by itself, prove a resolver is *authored* correctly for every domain-specific edge case (e.g., a resolver that never attempts to walk a particular relationship at all, rather than walking it and losing rows to RLS) — that class of defect is caught only by each adopting domain's own adversarial test suite for its own resolver, not by the shared mechanism. §W15.1.9 states this same limitation in its own terms. |

**Summary, stated as the directive requires — plainly, not as a
blanket claim:** every one of the eight directive categories has at
least one closing test in this design. Two categories carry a named,
accepted residual: **undisclosed** beneficial interest (no detection
mechanism exists or is claimed), and **resolver-authoring correctness
beyond undercounting** (caught by per-domain test suites, not by the
shared mechanism). Both residuals are pre-existing limitations already
stated in §W15.2.5 and §W15.1.9 respectively, not new gaps this
subsection discovers — this subsection's contribution is making the
mapping from the directive's own categories to those limitations
explicit, per its instruction to "be honest; do not claim closure you
can't support."

### W15.3 DEP-CRM-4 — formal decision

> **DECISION (`security`, owner of DEP-CRM-4):** doc 31 §12.2's proposed
> permission set is **accepted with three amendments**, and the
> mass-action blast-radius control set is fixed as `CRM-BR-1` … `CRM-BR-6`
> below. This decision does **not** cover marketing consent (DEP-CRM-1,
> `identity-compliance`'s) — CRM stays fail-closed until that exists, and
> nothing here should be read as clearing it.

**Permissions.** `crm_config:read`, `crm_config:manage`, `crm:read`,
`crm:send`, `crm:approve` — accepted as named. Amendments:

1. **`crm:send` splits.** `crm:send` covers a send to an individually
   named player in a support context; **`crm_bulk:execute`** covers any
   send whose recipients are *resolved* rather than hand-enumerated. Same
   reasoning as `bonus_bulk:execute` (§B1.2 item 2): a per-send authority
   applied to a resolved audience is not a control, it is an accounting
   error waiting for reconciliation to find it. `crm_bulk:execute` is
   **always** four-eyes, regardless of audience size.
2. **Role-wiring constraints, enforced in code and tested** — not
   sentences in a document:
   - No role holds both `crm_config:manage` and `crm:approve` (doc 31
     already states this; restated as a mechanical constraint).
   - No role holds both `crm_config:manage` and `crm_bulk:execute`.
   - **No role holds `crm:approve` together with `bonus_offer:manage` or
     `bonus_campaign:activate`.** Without this, the CRM approval and the
     Bonus approval on the same mass grant are the same human, and the
     two-domain control chain collapses to one pair of eyes.
3. **`crm:read` is per-field gated**, exactly as §B1.3 requires for
   `bonus:read`: a `crm:read` holder without `verification:read` /
   the RG read permission sees a generalized `suppressed`, never a
   KYC- or RG-derived suppression reason. A journey history is otherwise a
   convenient side channel around both.

**Blast-radius controls (binding):**

- **`CRM-BR-1`** — the audience is resolved, materialized, hashed and
  **disclosed at approval time**, and the hash plus row count is pinned
  into the approval payload. Identical mechanism to §B1.2 item 2's
  recipient-set pin; not a second design.
- **`CRM-BR-2` — the volume control attaches to journey/campaign
  activation, not to the per-player call.** This is SEC-W15-02's required
  fix, restated as a contract item. `RequestOfferGrant` is per-player and
  parameter-free, so N individually-sub-threshold calls escape every
  threshold that exists — the control must fire on the **activation of a
  journey containing an `offer_request` step**, gated on
  `resolved_audience_size × max_per_player_reward_value(offer_version)`,
  and it must be **enforced in Bonus**, which owns the economics and the
  approval tables, not in CRM. Concretely: `RequestOfferGrant` must refuse
  any request whose `trigger_reference` names a journey/step whose
  activation approval has not been consumed **for the pinned audience
  hash**. That makes the per-player call structurally incapable of being
  the first authorization of value. Routed as **REQ-CRM-VOL-1**
  (`architect`, doc 31) and **REQ-BONUS-VOL-1** (`bonus-engine`, doc 10
  N2.4). **SEC-W15-02 is not closed until both adopt it.**
- **`CRM-BR-3`** — dry-run/preview resolves the audience and grants and
  sends nothing, **and is itself audited**: a preview is a bulk read of
  player data and an unaudited preview is an unlogged mass export.
- **`CRM-BR-4`** — kill switch halting a running campaign is
  **single-actor, no four-eyes** — the fail-closed direction, per §B1.2's
  standing asymmetry.
- **`CRM-BR-5`** — preference-centre and unsubscribe tokens: single-
  purpose, ≥128 bits of CSPRNG entropy, **stored hashed**, bound to
  `(tenant, player, purpose)`, expiring, revoked on use for one-shot
  purposes, and **never containing or derivable from a player id**. The
  endpoint must not be an enumeration oracle: identical response and
  identical timing envelope for valid, invalid and expired tokens.
- **`CRM-BR-6`** — `SEP-1` adoption at the `offer_request` targeting point
  (REQ-SEP-CRM-1).

### W15.4 Three amendments to the Wave 1 §B1 contract

#### W15.4.1 `SEP-1` is now a standing precondition of §B1.2

Every operation listed in §B1.2 is additionally subject to `SEP-1`
(§W15.1), which is **not** threshold-gated and is **not** satisfied by the
four-eyes check. Where §B1.2 item 3's governance trigger compares
requester and approver to each other, `SEP-1` compares both of them to the
*beneficiary*. §B1.2 item 3 stands unchanged; `SEP-1` is added beside it.

#### W15.4.2 `pending_settlement` extension to §B1.2 item 7 (closes SEC-W15-09 item 3)

§B1.2 item 7 gates four-eyes on a Grant being in `completed`, on the
reasoning that a `completed` Grant is an *earned* entitlement and
cancelling it is confiscation. `pending_settlement` (doc 10 §N1.4) did not
exist when that was written, and it breaks the predicate in two distinct
ways:

**(a) Cancellation/forfeiture of a Grant in `pending_settlement` requires
four-eyes**, above the same configurable threshold as item 7, and with a
reason code that is distinguishable from the original terminal trigger's
reason code. A `pending_settlement` Grant has open attributable exposure
(`AOE ≠ ∅`) — its fate is decided but its value is not yet extinguished,
and some of that value may still legitimately resolve to the player (the
late-win case N1.7 and G-2 are about). A single actor acting on a deferred
Grant can take value that the deferral exists precisely to protect. Under
item 7 as written, that Grant is not `completed`, so it falls into the
"pre-`completed`, single-actor" branch — which is the gap.

**(b) Any *staff-initiated* severity upgrade of `terminal_resolution`
requires four-eyes at threshold 0 — always.** N1.4 step 6 permits
`terminal_resolution` to be upgraded to a more severe value and never
downgraded. N1.4 itself states that the reason code is what "a disputing
player's case and any compliance reporting turn on". A single actor able
to upgrade `expired` → `forfeited` after the fact can retroactively
recharacterize a neutral expiry as an abuse finding, with both a financial
consequence (forfeiting what would otherwise resolve to the player) and a
regulatory one (an abuse finding in a compliance report that no second
person saw). A **system**-driven upgrade from an automated detector is
*not* four-eyes-gated — it is an automated trigger, not a discretionary
act — but it must record the triggering detector and its rule version, and
a staff-initiated upgrade must be **distinguishable in the trail from a
system one**. That means each upgrade event carries its own `actor_type`;
`terminal_resolution`'s current value alone cannot answer "who decided
this".

**(c) The no-downgrade rule must be a database constraint, not
application logic.** N1.4's "never downgrades" is currently a design
statement. It must be a severity-ordered lookup plus a trigger refusing
any decrease — otherwise the most attractive single-actor manipulation
(downgrade `forfeited` → `expired` to make an abuse finding disappear
before a dispute) is prevented only by discipline in application code,
which CLAUDE.md rules out for exactly this class of invariant.

**New audit rows implied** (additions to §B1.4, not replacements):
`bonus_grant.settlement_deferred` (system; after-state enumerating the
outstanding exposure components, by id, never by attribute) and
`bonus_grant.terminal_resolution_upgraded` (staff **or** system; reason
code always; before/after `terminal_resolution`, plus the approval
references when staff-initiated).

**Related re-audit requirement, routed.** Every existing
status-branching predicate must be re-read against `pending_settlement`.
From a security standpoint three matter and are named here:

| Predicate | Requirement | Routed to |
|---|---|---|
| §B1.2 item 7's own `completed` test | Fixed by (a) above | `security` (done here) |
| The self-exclusion open-bet enumeration (migrations `0043`/`0049` family, ADR 0034 §14) | A `pending_settlement` Grant must **not** be invisible to the enumeration. An invisible deferred Grant is a self-exclusion that silently fails to cover live exposure — and per S-9, a dropped enumeration leaves no evidence it was incomplete | **REQ-PS-ID-1**, `identity-compliance` |
| The player-facing Grant projection (§B1.3's curated projection) | `pending_settlement` must map to a player-meaningful state; it must **not** display as "active" (dishonest), and must **not** expose `terminal_resolution` or `terminal_trigger_reason_code` before the resolution is final (it discloses a pending abuse finding to the subject of that finding, before review completes) | **REQ-PS-BONUS-1**, `bonus-engine` |

#### W15.4.3 `bonus_suggestion:create` / `bonus_suggestion:review` (closes SEC-W15-20)

Minted now, as amendments to §B1.1's table:

| Permission | Gates | Four-eyes above tier? |
|---|---|---|
| `bonus_suggestion:create` | Write a `BonusSuggestion` row with `originating_kind` = `rule` or `model`. **Service identity only** (`auth.PrincipalService` / `audit.ActorService`) — held by **no** human role | n/a |
| `bonus_suggestion:review` | Claim, annotate, edit, approve or reject a suggestion; also create a suggestion with `originating_kind = manual`. Confers **no** power to activate anything | No |

**Why `originating_kind = manual` sits under `:review`, not `:create`.**
A staff member proposing a suggestion is a reviewer-class act. If `:create`
had to accommodate a human, it would need a human role holder and its
service-identity-only property — the property that makes it safe to grant
at all — would be gone. Stated explicitly so the obvious-looking
"manual means create" reading is not adopted later by default.

**Binding wiring constraints:**

1. **`bonus_suggestion:create` is granted to no role.** The permission is
   absent from every entry of `permission.go`'s role map, and the service-
   identity path is its only grant. Test: no role in the map contains it.
2. **`bonus_suggestion:review` is never bundled with `bonus_bulk:execute`
   or `bonus_grant:issue`.** *Concrete failure scenario*: doc 10 N3.2
   states that Activation **is** `BulkGrantJob.Create()` / `Grant.Issue()`
   called with an extra `originating_suggestion_id` parameter. If one
   principal can approve a suggestion and then activate it, the review
   step adds no eyes at all — and it is **worse than no control**, because
   the resulting trail *looks* reviewed: a self-authored justification
   attached to a grant the same person issued. Wiring:
   `bonus_suggestion:review` → `RolePromotionsManager`;
   `bonus_bulk:execute` / `bonus_grant:issue` stay on
   `RoleBonusOperations`. §B1.1's hard constraint 1 (no principal holds
   both roles) already separates the humans; this adds the permission-level
   constraint so a future role redesign cannot quietly recombine them.
3. **Activation is not a new authority and consumes no new approval.** It
   consumes the **existing** four-eyes control on whatever it activates —
   a bulk activation is always four-eyes (§B1.2 item 2), a single grant is
   gated at the per-grant threshold (item 1). A suggestion's approval
   **never** substitutes for that approval. Restated because N3.2's "with
   an extra parameter" phrasing makes the opposite reading easy and
   attractive.
4. **`SEP-1` applies at both review and activation.** A reviewer may not
   approve a suggestion whose `proposed_player_population` resolves to
   include their own `Person`; an activator may not activate one.

**Audit amendment to §B1.4.** The `bonus_suggestion.generated` row lists
actor types "system, staff"; amended to **`service`** where a service
principal exists (a rule or model runner authenticating as one), with
`system` reserved for the case where genuinely no principal does. This
follows doc 10 N2.3's own correction of actor typing against
`internal/audit`, where `ActorSystem` requires `ActorID == uuid.Nil`.
`bonus_suggestion.reviewed` (claim/edit/approve/reject, reason code
mandatory on reject) is added alongside `.generated` / `.actioned`.

§B1.3's `bonus_suggestions` row — staff-only per-command policies, no
player policy — stands unchanged and is reaffirmed: a player-visible
suggestion leaks the operator's segmentation model back to the player.

### W15.5 Eligibility Decision Record — security constraint set (DEP-SEG-1)

The EDR (doc 30 §7) is the most sensitive **new** artifact in this gate:
by construction it carries the RG decision and effective restriction
policy identity, the KYC `VerificationStatus` and tier, the Risk decision
**with matched rule ids and versions**, and the jurisdiction resolver's
source — per player, per checkpoint, forever, append-only. DEP-SEG-1
correctly leaves its *physical shape* to `bonus-engine` and
`ledger-finance`. The five constraints below bound that choice; they are
`security`'s and are not negotiable at the physical-design level.

**`EDR-S1` — staff-only, no player policy, append-only.** Whatever shape
is chosen (columns on `bonus_progress`, a `bonus_eligibility_decisions`
child table, or a structured `decision_evidence` document), the EDR's RLS
shape is **`bonus_progress`'s, not `bonus_grants`'**: `tenant_id NOT
NULL`, `ENABLE` + `FORCE ROW LEVEL SECURITY`, per-command staff-only
policies each carrying the `app.player_account_id IS NULL` conjunct,
**no `player_self_scope` policy at all**, no `DELETE` policy,
`ledger_deny_mutation()` on `UPDATE OR DELETE`, and a
`BEFORE TRUNCATE … FOR EACH STATEMENT` deny trigger.

*The constraint this places on DEP-SEG-1's open choice*: **the EDR must
not be stored on any table that carries a player-readable policy.** If it
lands as columns on `bonus_progress`, it inherits the correct shape
automatically (§B1.3 already denies that table a player policy, for the
same reason). If it lands on `bonus_grants` — which **does** carry
`player_self_scope FOR SELECT` — it is a disclosure by construction, with
no code change required to exploit it: the player's own existing
self-service read returns it. This is the one place in this section where
`security` constrains a decision that is otherwise `bonus-engine`'s and
`ledger-finance`'s, and the reason is that the alternative is unrecoverable
by any later application-layer fix.

**`EDR-S2` — per-field read gating; `bonus:read` alone is not sufficient.**
Reading the EDR requires `bonus:read` **plus**, per field group:

| Field group | Additional permission required |
|---|---|
| RG decision code, effective restriction/self-exclusion policy identity | `rg_restriction:read` (verified live in `internal/auth/permission.go` as `PermRGRestrictionRead`; if the RG read surface is renamed or split later, **a permission name that cannot be resolved fails closed — it does not default to visible**) |
| `VerificationStatus`, `kyc_tier` | `verification:read` |
| Matched Risk `rule_id`s and rule versions | `risk_config:read` |

A caller holding only `bonus:read` sees the **eligibility outcome and a
generalized reason code**, never the derived source fields. This extends
§B1.3's existing `awaiting_verification` rule from a single status value
to the whole record.

*Concrete failure scenario if omitted*: §B1.1 grants `bonus:read` to
`RoleSupport`, which today holds **only** `PermPlayerRead` (verified live
in `internal/auth/permission.go`) — none of `verification:read`,
`risk_config:read` or `rg_restriction:read`. Without `EDR-S2`, a support
agent reads, for any player in the tenant, the RG restriction policy in
force, the KYC tier, and the tenant's own matched risk rule ids — three
permission boundaries crossed through one bonus endpoint.

Enforcement is a **server-side projection**, not response filtering: the
query must not select fields the caller cannot read. A field filtered
after selection has already reached query logs, error payloads, traces and
any cache in between.

**`EDR-S3` — rule id + version is acceptable stored, never player-facing.**
Storing matched `rule_id` + `rule_version` is *required* for reconstruction
(doc 30 §7.2) and is acceptable **at rest**, because `EDR-S1` makes the
record staff-only. It must never appear in a player-facing dispute export.
The exportable projection is: the decision outcome, a **player-meaningful**
reason code, the offer/campaign version identities, the amounts, the
timestamps. It is **not**: rule ids, rule versions, thresholds, criteria
hashes, segment ids or versions, evaluator versions, model versions, or
the jurisdiction resolver's internal source.

*Reason*: §B1.4 already bans a Risk rule's `Threshold` from the audit
trail on exactly this logic. The EDR is where that ban would otherwise be
circumvented, because a dispute export is the one legitimate path by which
staff-only bonus data is deliberately handed to a player. A player who
learns the exact rule id and version that denied them learns the shape of
the control set, and learns more of it with every dispute they raise.

*Corollary, flagged not minted*: a **per-player** dispute export needs its
own authority. `bonus_report:read` is deliberately aggregate-only (§B1.1)
and `bonus:read` is a staff read, not an export. Either a
`bonus_dispute:export` permission is minted, or the export runs through
doc 16's data-subject-access path. Not decided here — it belongs with doc
16's DSAR design, and `security` records it as **OI-SEC-W15-A**, open.

**`EDR-S4` — no PII, no evidence, no membership.** Doc 30 §7.2's "never
any document, evidence, or PII" for KYC is restated and extended: the EDR
records segment **ids and criteria hashes**, never resolved membership
lists and never the attribute *values* that made the player match. This is
§B1.4's segment-membership rule applied to the EDR, and it matters more
here, because the EDR is per-player by design and retained for the same
5–7 years — a PII copy that outlives every erasure request made against it.

**`EDR-S5` — every cross-reference is composite-FK'd.** The EDR names
`segment_version_id`, Risk rule versions, `offer_version_id`,
`campaign_version_id`, the `TenantJurisdictionConfig` version — all
tenant-owned. Each must be `FOREIGN KEY (x_id, tenant_id) REFERENCES
x (id, tenant_id)` per §B1.3's rule. A plain single-column FK lets tenant
A's EDR reference tenant B's rule version, which then leaks through any
join that resolves the reference for display — a cross-tenant disclosure
arriving through a legitimate read path.

**Tests `qa` must include for the EDR:**

- A player-scoped connection reads **zero** EDR rows, for their own grant
  and for anyone else's.
- A tenant B staff token reads zero of tenant A's EDR rows.
- A `bonus:read`-only token receives the outcome and generalized reason
  code and **none** of the RG / KYC / Risk-rule fields — asserted
  field-by-field, not by a single "response looks fine" check.
- A `bonus:read` + `verification:read` token sees KYC fields and still
  **not** Risk rule ids.
- An unresolvable/renamed permission name in the gating table denies
  rather than defaults to visible.
- An EDR row cannot be `UPDATE`d or `DELETE`d; `TRUNCATE` raises.
- A dispute export for a real decision contains **no** rule id, rule
  version, threshold, criteria hash, segment id or evaluator version —
  asserted as an explicit denylist over the serialized output, not by
  inspection.
- An EDR row cannot be written referencing another tenant's
  `segment_version_id` / `offer_version_id` / rule version.

### W15.6 What this section does not cover

- **It reviews no code.** None exists for any subsystem named here. Every
  statement is a design-level contract.
- **SEC-W15-02 is not closed.** §W15.3's `CRM-BR-2` states the required
  fix; only `architect` (doc 31) and `bonus-engine` (doc 10 N2.4) can
  implement it, in their own dispatches. `security` will re-verify.
- **`SEP-1`'s underlying primitive is unconfirmed by its owner.**
  Designed against `staff_users.person_id` ↔ `PlayerAccount → Person` as
  `security` understands it; `identity-compliance`'s 4HB1FW-05 dispatch
  confirms or corrects it (REQ-SEP-ID-1).
- **DEP-AFF-5 (node-subtree RLS) is not designed here** — it is a
  precondition (`AFF-C2`), not a deliverable of this section.
- **Undisclosed beneficial ownership is not solved** (§W15.2.5's stated
  limitation), and **affiliate-side entity resolution is deliberately not
  built** — `AFF-4E-1` is specified precisely so that it is not needed to
  close the P0.
- **SEC-W15-05 … SEC-W15-13 remain open**, including the `tracking_token`
  subject-binding and key-rotation gaps, the unauthenticated `Click` write
  path, the `pending_settlement` ring-fencing G-3-family item,
  `CustomerProfile`'s unbounded read, the CRM contact-endpoint
  revalidation gap, and `AttributionCandidate`'s access-model conflict.
- **No human decision is selected**: G-2, the `OpenBetSelfExclusionPolicy`
  default, the mixed/bonus-funded cashout policy and FD-1 are untouched
  by this section, and none of the controls above presupposes any
  particular answer to them.
- **Nothing here is a claim that any subsystem is secure**, and passing
  these controls once is not a standing clearance — per
  `.claude/agents/security.md`, each implementing wave is reviewed on its
  own.
- **Fix Round 2 (§W15.1.9–§W15.1.12, §W15.2.7–§W15.2.8) closes four named
  defects at the design level and no further**: `RK-W15P2-1`,
  `RK-W15P2-6`, `RK-W15P2-7`, and `NEW-6`. It carries forward, honestly,
  two residuals stated in place rather than repeated here: the
  separate-transaction denial-audit record can still lose evidence of an
  attempt (not the attempt's effect) to a crash in a narrow window
  (§W15.1.11), and undisclosed beneficial ownership remains undetected
  (§W15.2.5, restated in §W15.2.8). This dispatch is `security`'s own
  fix to its own prior round's design and is, per its own tasking,
  subject to independent re-review by `risk` and `code-reviewer` before
  either finding is marked closed in fact rather than at the design
  level.
- **Fix Round 2, closing pass (Wave 1.5, Fix Round 2 Round 2) corrects
  two defects in that same dispatch, found by `risk` and
  `identity-compliance` re-reviewing it, not by a new external finding**:
  (1) `risk`'s partial re-open of `RK-W15P2-1` — §W15.1.9's cardinality
  `CASE` had no fail-closed default and was proven silently inert on
  exactly the resolver shape (Affiliate's reflexive ancestor closure) it
  was written to catch; fixed by §W15.1.9's second pass (a fail-closed
  `IF/ELSIF/ELSE`, a named `ancestor_closure` shape, a new Step 0b
  subtree-scope self-proof, and a `SECURITY DEFINER` explicit-tenant-
  predicate construction for that resolver, in place of an independent
  count that would have shared the same RLS blind spot it was meant to
  check). (2) `identity-compliance`'s non-sign-off of `REQ-SEP-STAFF-1` —
  the proposed `staff_users` constraint and its per-tenant remediation
  framing could not see platform-scoped (`tenant_id IS NULL`) staff at
  all; fixed by a two-pass remediation query and a confirmed, corrected
  `platform_admin` exemption on the constraint, which in turn required
  correcting step 4's own tenant-match comparison (bare `<>` to `IS
  DISTINCT FROM`) because the exemption's soundness depends on step 4
  actually refusing every `platform_admin` actor rather than silently
  passing one. **One dependency remains open, routed rather than
  resolved**: `architect`'s confirmation that `agentnetwork` parent/child
  edges never cross a tenant boundary (§W15.1.9, second pass) — until
  confirmed, `REQ-SEP-AFF-1`'s `ancestor_closure` construction is not
  claimed complete. This closing pass is, again, `security`'s own
  correction to its own immediately-prior design and remains subject to
  the same independent re-review by `risk` and `code-reviewer` before
  either finding is marked closed in fact.

---

## Stage 4H-B1 Wave 3 Phase 6 — code-level security review of the Bonus governance / EOI surfaces

**Reviewer:** `security`. **Scope reviewed:** the surfaces Wave 3 Phases 2–5
added — `internal/bonus/four_eyes_ops.go`,
`internal/bonus/change_governance.go`,
`internal/httpserver/bonus_governance_handlers.go`,
`internal/httpserver/bonus_domain_ops_handlers.go`,
`internal/httpserver/bonus_routes.go`, `internal/economicop/enforce.go`,
migration 0063, and the three new sweep/scheduler jobs
(`deposit_sweep.go`, `cashback_scheduler.go`, `expiry_sweep.go`,
`schedulers.go`). **Explicitly NOT in scope of this review:**
`internal/risk`, `internal/rg`, `internal/kyc`, CRM/Affiliate/
Gamification/Reward-Orchestrator, sportsbook, retail, any UI, and any
real external provider. This is a code-level and design-level review
appropriate to a development-stage platform — **not** a penetration test
and **not** a certification-grade audit, both of which require external,
human-run engagements.

### W3P6.1 — SEC-W15-02 / CRM-decomposition re-test

The four named vector shapes (decomposition; pagination laundering;
payload substitution; actor/subject laundering) were re-tested against
each of the five surfaces Wave 3 added (`manual_grant_issue`,
`bulk_job_execute`, `campaign_activate`, `offer_publish`, and the
EOI-minting endpoint). All twenty cells now have executing integration
coverage, across
`internal/bonus/wave3_security_adversarial_integration_test.go`,
`internal/bonus/wave3_security_matrix_integration_test.go`,
`internal/bonus/four_eyes_ops_integration_test.go`,
`internal/bonus/lifecycle_integration_test.go`, and the two
`internal/httpserver/bonus_eoi_mint_*` suites.

Three defects were found and fixed during this phase (commit `ab8ee70`):
`ConsumeRootBudget` never enforced doc 34 §3.2's subject-set containment
for `single_subject` scope; `ExecuteBulkGrantJobWithApproval`'s four-eyes
payload pinned nothing about what was approved; and the EOI-minting
endpoint treated the budget-bounding fields as optional. Two further
defects were found and fixed afterwards — see W3P6.2.

### W3P6.2 — Findings on the governance HTTP surface

- **`threshold_at_decision` / `amount_at_decision` were client-supplied
  (Medium, FIXED).** Migration 0063 created those two columns so a later
  threshold change cannot retroactively make a past decision look
  compliant. Accepting them from the request body handed authorship of
  that append-only, deny-update/deny-delete-protected forensic record to
  the very principal it exists to hold to account, and their optionality
  additionally allowed the record to be left blank. Both are now resolved
  server-side, inside the same transaction, from `ResolveApprovalPolicy`
  and the request's own `amount_at_request` — the same construction
  `internal/withdrawal.Approve` already uses, which migration 0063 itself
  names as its model. `reason_code` is now required for a reject.
- **`amount_at_request` could never be written (Medium, FIXED).**
  `scanChangeRequest` scanned that `NUMERIC(38,0)` column straight into a
  `**big.Int`, which only works while the column is NULL. Filing any
  change request carrying an amount — i.e. exactly the above-threshold
  requests whose amount is the load-bearing input to both the threshold
  and the forensic record — failed with a 500 at the `RETURNING` scan.
  Now scanned via `pgtype.Numeric`, as every other monetary column in the
  package is.

### W3P6.3 — RBAC, tenant scoping and RLS

Confirmed for every route in `registerBonusRoutes`: `tenant_id` is
derived only in `auth.Middleware` from the verified JWT and is never read
from a header, path or body; every staff route is additionally wrapped in
`auth.RequireTenantScope` (denying nil-tenant platform principals) plus
either a static `auth.RequirePermission` or, on the two routes whose
required permission depends on a body field, an equivalent in-handler
`RoleHasPermission` check against `changeOperationPermission` /
the `operation_type` switch. A client-supplied `brand_id` cannot name
another tenant's brand: every Bonus table carries the composite
`FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)`.

Confirmed for RLS: every new HTTP handler and every new sweep/scheduler
job performs all tenant-owned reads and writes inside `pool.WithTenant`.
The single cross-tenant read (`allActiveTenantIDs`, `WithoutTenant` over
the platform-level `tenants` table) mirrors `internal/reconciliation`'s
established pattern exactly, and every per-tenant tick runs under
`WithTenant(tenantID)` with a `pg_try_advisory_xact_lock`. No new code
path uses `WithPlatformAdmin` or an RLS-exempt connection.

### W3P6.4 — Confirmed-safe failure modes

`POST /v1/admin/bonus/bulk-jobs/{jobID}/execute` always fails today
because `newCreateBulkGrantJobHandler` never populates
`bulk_grant_jobs.parent_operation_id` (Phase 4's F3; `bonus-engine`'s to
close). Reviewed as a security question — does the failure leak state? —
and confirmed fail-closed and atomic: the four-eyes approval is consumed
and the job flipped to `running` *before* the guard fires, but
`WithTenant` rolls the whole transaction back, so the approval is not
burned (which would be unrecoverable — migration 0063 forbids any
transition out of `applied`), the job stays `queued`, and no job item,
Grant or ledger entry survives. Asserted by
`internal/httpserver/bonus_bulk_job_execute_failclosed_integration_test.go`.

### W3P6.5 — Residual items, named rather than closed

1. **The EOI budget bound lives only in the HTTP handler.**
   `newMintEconomicOperationHandler` now requires `asset_code`,
   `intended_aggregate_value` and a positive `recipient_ceiling`, but
   `bonus.MintRootOperation` itself still accepts nil for all three, and
   `economicop.ConsumeRootBudget` treats a nil ceiling as "no recipient
   check" and a nil aggregate as "no value check". Any future non-HTTP
   minting path therefore reopens the decomposition vector at the
   mechanism's own entry point. Making the bound structural (enforced in
   `MintRootOperation`, or as a DB CHECK on `economic_operations` for the
   Bonus operation types) is the durable fix, but `MintRootOperation` is
   `bonus-engine`'s primitive and doc 34 permits nil bounds for operation
   types other than Bonus's two — **routed to `bonus-engine` and
   `architect`, deliberately not changed unilaterally here.**
2. **Subject-set containment is enforced for `single_subject` only.**
   `enumerated_set`/`criteria_defined` roots remain bounded by
   `recipient_ceiling` alone, because no consumer materializes doc 34
   §2.2's `subject_set_hash`/`subject_definition_hash` on the EOI row.
   The bulk surface's recipient set is bound instead by
   `BulkJobExecutePayloadMatch`'s recipient-set hash in the four-eyes
   payload and by SEP-1 — adequate today, but the EOI row itself does not
   bind it.
3. **`ConsumeRootBudget` skips the subject check when the caller passes
   `uuid.Nil`.** No caller in the repository does, so this is latent, not
   live — which is also why it carries no regression test. A future
   caller passing a zero subject would silently bypass the containment
   check; refusing a nil subject under `single_subject` scope would close
   it.
4. **The EOI-mint audit record is thin.** `MintRootOperation` audits with
   only `operation_type`; the handler adds no IP, user-agent, request id,
   or the authorization bounds themselves. An investigator asking "who
   authorized this budget, from where, for what" cannot answer it from
   the audit trail alone.

**Nothing in this section is claimed to make the reviewed surfaces
"secure" in general.** It records what was examined, what was found, and
what deliberately was not examined.

---

## Stage 4I — jurisdiction resolution: audit/provenance, tenant isolation, and the adversarial test contract

**Author:** `security`. **Origin:** this section folds
`docs/governance/stage-4i-security-model.md` §S-2 (audit and provenance
content), §S-3 (the RLS / tenant-isolation contract) and §S-6 (the
adversarial test specification) into this permanent document, per
`DR-4I-SEC-01`. That routing was `architect`'s explicit decision in
`docs/governance/stage-4i-canonical-model.md` §13.6: the stage-4I
governance document is a *stage* artefact, these rulings are *permanent*
platform rules, and the Stage 4I final independent security/compliance
review owns this document.

**This is not a transcription.** The stage document was written in Phase 4,
*before* the code existed. This section states the rules as they stand
after the whole stage shipped, with every place the original reasoning was
wrong or unachievable corrected in place and marked. Where a rule is
specification rather than behaviour, it says so — per CLAUDE.md's
no-fake-completion rule, a binding requirement and an implemented control
are different things.

Section numbering is `J4I.*`. Cross-references to `§S-n` mean the stage
document; `§n` means the canonical model.

### J4I.1 Two artefacts, deliberately separate (`AR-1`, `AR-2`)

**`AR-1` (binding).** The per-operation resolution record lives in its own
append-only, tenant-scoped table (`jurisdiction_resolutions`), **not** in
`audit_log`. Three reasons, all still valid:

1. **Volume.** Resolution happens on every launch, every bet, every
   deposit, every gate checkpoint — strictly more often than
   `bonus_grant.progressed`, which §B1.4 already flags as a per-bet-class
   event that would dominate `audit_log`.
2. **Access control.** `audit_log` is readable by any `audit:read` holder
   in the tenant (migration 0014's `dual_scope_isolation`). A resolution
   record carries the basis selected and the bases rejected, which is
   KYC/identity-adjacent. §B1.3's rule — "`bonus:read` must never become a
   side channel around `verification:read`" — applies here directly.
3. **Referential integrity.** An operation row must be able to carry a
   **foreign key** to the resolution that governed it.
   `audit_log.target_id` is `TEXT` with no FK and is not a usable anchor.

**`AR-2` (binding, NOT YET IMPLEMENTED).** Every operation that consumes a
jurisdiction persists a reference to the resolution that governed it,
inside the same transaction as the effecting write. The two existing
snapshot columns (`casino_launch_sessions.jurisdiction_code`,
`bonus_grants.jurisdiction_code`) stay and gain a
`jurisdiction_resolution_id` FK alongside — storing only the code preserves
the original defect in a new form: you know *which* jurisdiction, never *on
what basis*.

> **Status, stated plainly.** `AR-2` is **NOT IMPLEMENTED**
> (`DR-4I-BE-02`), and `jurisdiction.Persist` has **zero production call
> sites** (`DR-4I-BE-01`), so `jurisdiction_resolutions` is never written
> outside tests. §5.1's "one row per resolution attempt, including
> failures" is therefore **specification, not behaviour**, as of Stage 4I's
> close. A future phase must not read this section and assume the table is
> populated. The deferral is deliberate and reasoned: in Stage 4I every
> player-scoped resolution is `unresolved(no_signal)`, so a per-launch row
> would be an unbounded, player-triggerable stream of identical rows into
> an append-only table with no delete path — the amplification hazard
> J4I.3 forbids, for less evidentiary value.

### J4I.2 What MUST be recorded

Per resolution, in `jurisdiction_resolutions`: `id`, `tenant_id` (RLS key),
`brand_id` (nullable, composite FK), `player_account_id` (nullable,
composite FK), `operation_class` (a closed enum, never a wire string),
`requested_by_actor_type` / `requested_by_actor_id` (matching `audit_log`'s
existing `CHECK` vocabulary, migration 0014), `outcome`,
`jurisdiction_code` (NULL iff outcome ≠ `resolved`, FK to
`jurisdictions (code)`), `selected_basis`, `considered_bases`,
`confidence_class`, `resolver_policy_version`, `registry_version`,
`config_effective_from`, `as_of`, `created_at`.

`outcome` is **three-valued, never two**. `unresolved` means "the inputs do
not determine a jurisdiction"; `refused` means "the resolver declined to
answer" (scope mismatch, unavailable dependency, precondition failure).
They have different remediations and must not be collapsed. `unresolved`
is a first-class recorded outcome, not an absent row.

### J4I.3 What must NEVER be recorded — and the principle that makes the boundary non-arbitrary

> **A resolution record persists the DECISION and REFERENCES to its
> evidence. It never persists the evidence VALUES.** Reconstruction is
> performed by re-reading the referenced evidence under *that evidence's
> own* access control and retention rule — never by reading a copy of it
> made into a store with different access control and a longer retention.

The obvious objection, answered so nobody has to re-derive it: the resolved
`jurisdiction_code` *is* itself recorded, and when residence is the
selected basis it approximately discloses residence. That is unavoidable
and correct — the jurisdiction **is** the decision, and a decision that
cannot be reconstructed cannot be defended to a regulator. What is not
unavoidable, and is therefore forbidden, is additionally recording the raw
inputs and the values of bases that were considered and *not* selected.
"A `kyc_corroboration` basis was consulted and disagreed" is a decision
fact. "It said `UA`" is a copy of a KYC-derived personal attribute, in an
append-only store, about a determination that did not even use it.

**The prohibited list. An implementation that records any of these is a
blocking finding.** This extends §B1.4's never-log list, which continues to
apply in full.

1. **`kyc_documents.issuing_country`'s value — never, under any
   circumstance, including when it was a corroborating input.** Record
   instead: the basis enum, `kyc_documents.id` as an `evidence_ref`, the
   `document_type`, and the agreement flag.
2. **A player's declared or verified residence country value.** Record the
   basis, the `player_accounts.id` (already the row's own key) and the
   capture/verify timestamp. The current value is always readable from the
   row under that row's own RLS; a copy in an append-only multi-year store
   is a PII duplicate that outlives every erasure request made against the
   original (the identical argument §B1.4 makes for segment membership).
3. **Nationality, in any form.**
4. **Any raw IP address, geo-coordinate, city, ISP, or geolocation-vendor
   payload as a jurisdiction-evidence field.** `audit_log.ip_address`,
   `sessions.ip_address` and `login_attempts.ip_address` already capture
   the request's IP under their own retention and access rules. A second
   copy — and worse, a *derived* precise location — is a new, more
   sensitive category of personal data created as a side effect of an audit
   requirement.
5. **`persons.person_key_hash`, or any cross-brand identity correlator.**
   `jurisdiction_resolutions` is tenant-scoped and readable by tenant
   staff; `persons` is deliberately platform-scoped (ADR 0015). Putting the
   cross-brand correlator into a tenant-readable table hands every tenant a
   join key for correlating the same human across other operators' brands —
   a **cross-tenant privacy leak with no attacker required**, and the single
   most likely accidental version of this mistake, because a future
   implementer will want it for reporting.
6. **Full name, date of birth, address, phone, email, document number,
   document image, or document URL** — restated from §B1.4 because
   "provenance" is precisely the word under which someone will propose
   attaching them.
7. **Vendor raw responses, verbatim.** Store the mapped enum plus the
   vendor's opaque reference id, never the response body (ADR 0028's rule
   for KYC vendor responses).
8. **Provider or vendor API credentials, HMAC secrets, or per-tenant
   geolocation-vendor keys.** A resolution record names the provider id,
   never the credential used to reach it.

> **Verified against the FINAL shipped schema** (Stage 4I closing review,
> `information_schema.columns`, not the migration text): the table has
> exactly 18 columns and **none** is on this list. The one free-form field,
> `considered_bases JSONB`, is structurally constrained in Go rather than by
> a `CHECK`: it is marshalled from `[]ConsideredBasis`, whose only two
> fields are the basis enum and a status enum, and whose value is set only
> inside the resolver. There is no exported path by which a caller can put
> an evidence value into it. `registry_version` carries a licence UUID, not
> a personal attribute. **No new KYC or PII field appears anywhere in the
> audit trail Stage 4I produced**, and no code path in the stage reads
> `kyc_documents.issuing_country` at all.

### J4I.4 The four `audit_log` event classes, and the amplification guard

`audit_log` — not `jurisdiction_resolutions` — receives an entry for, and
only for:

1. **`jurisdiction_registry.*`** — a write to `jurisdictions`, `licences`,
   `tenant_jurisdiction_configs`, or (added Stage 4I Phase A)
   `tenants.licence_id` — the tenant-licence binding, a distinct target from
   the registry rows themselves but audited under the same family since it
   is the same class of platform-admin registry-adjacent mutation. Staff
   actor, before/after in metadata, **reason code required**.
   *(IMPLEMENTED: `jurisdiction_registry.jurisdiction_created` /
   `jurisdiction_registry.licence_created` /
   `jurisdiction_registry.tenant_licence_assigned` — the last one, unlike
   the first two, is written TENANT-scoped (`audit_log.tenant_id` = the
   affected tenant, not `NULL`), because its subject is a specific tenant
   that must be able to read its own audit trail via `PermAuditRead`; see
   `docs/plans/stage-4i-jurisdiction-implementation-plan.md` Phase A and
   `docs/governance/task-registry.md`'s `PHASE-A-SEC-1` finding.)*
2. **`jurisdiction_fact.corrected`** — a staff correction to a player's
   jurisdiction-determining identity fact. Reason code required;
   before/after recorded **as a reference and a change flag, not as the two
   country values** (J4I.3 item 2 applies to `audit_log` at least as
   strongly as to the resolution table). *(NOT IMPLEMENTED — no such fact
   exists yet; blocked on HDR-J-3.)*
3. **`jurisdiction_resolution_active.changed`** — enabling or disabling
   resolution for a `(tenant, operation_class)` pair. Reason code required.
   Disabling it is a control-weakening act and must be as visible as using
   it. *(IMPLEMENTED, with before/after and a mandatory reason code —
   SEC-4I-F5.)*
4. **`jurisdiction.resolver_unavailable`** — resolver/dependency
   unavailability, **aggregated**. *(NOT IMPLEMENTED.)*

Per-request denials caused by an unresolved jurisdiction are **not**
individually written to `audit_log`. They are recorded by the existing
per-domain denial mechanisms plus the `jurisdiction_resolutions` row.

**Audit amplification is an attack, not just a cost.** If every resolver
failure wrote an `audit_log` row, an attacker — or one degraded dependency —
who can make the resolver fail can make the platform write one immutable,
never-deletable row per attempt. `audit_log` is append-only by trigger;
rows cannot be pruned by the application even deliberately. A sustained
failure therefore converts a transient outage into **permanent,
unreclaimable growth on the platform's most retention-sensitive table**,
and drowns genuine security events in noise during exactly the window
someone is reading them.

> **Requirement (binding).** `jurisdiction.resolver_unavailable` is emitted
> **at most once per `(tenant, operation_class, time_bucket)`**, carrying a
> count — never once per failed request. `qa` must include a test that N
> failed resolutions in one bucket produce exactly one `audit_log` entry.
> **Status: the event is NOT IMPLEMENTED, so the guard is unexercised.**
> This is currently harmless *because* nothing emits the event; it becomes
> load-bearing the moment anything does, and the aggregation must land in
> the same change that first emits it, never after.

### J4I.5 Universal RLS rules for every tenant-owned jurisdiction table

`backend` implements against this list; `qa` asserts it.

- **`tenant_id UUID NOT NULL`**, plus **`ENABLE ROW LEVEL SECURITY` and
  `FORCE ROW LEVEL SECURITY`**. FORCE is the load-bearing half: the
  application role owns these tables and bypasses non-FORCE policies
  entirely.
- **No `BYPASSRLS` assumption anywhere.** The application role is
  `NOBYPASSRLS` by construction (`deploy/init-app-role.sql`) and every
  read/write goes through `db.Pool.WithTenant` / `WithPlayerScope` /
  `WithPrincipalScope` / `WithoutTenant` / `WithPlatformAdmin`.
- **Composite foreign keys, never plain ones** —
  `(brand_id, tenant_id) → brands (id, tenant_id)` and
  `(player_account_id, tenant_id) → player_accounts (id, tenant_id)`
  (migration 0043's precedent). A plain `brand_id REFERENCES brands(id)`
  does not prevent tenant A's row from naming tenant B's brand; the
  composite FK makes cross-tenant attachment structurally impossible rather
  than policy-dependent.
- **Per-command policies. No `FOR ALL` policy. No DELETE policy. No UPDATE
  policy on `jurisdiction_resolutions`.** A resolution row is the *record of
  a decision*: an UPDATE is a rewrite of history and a DELETE removes the
  only evidence a control ran.
- **Append-only is enforced by a `BEFORE UPDATE OR DELETE` trigger, not
  only by the absence of a policy** — a trigger is not bypassed by table
  ownership, which is precisely why `audit_log_immutable` exists.
- **Plus a `BEFORE TRUNCATE … FOR EACH STATEMENT` deny trigger, on every
  such table without exception.** *(Corrected in the closing review —
  SEC-4I-F8. This bullet had been read as applying only to the append-only
  table. It does not, and the reason is mechanical: **PostgreSQL RLS does
  not apply to TRUNCATE at all.** TRUNCATE is governed solely by the
  TRUNCATE privilege, which the application role holds implicitly by owning
  the tables. Every per-command policy, every tenant predicate and every
  deliberately-absent DELETE policy on a table is therefore bypassed by one
  statement from an ordinary tenant-scoped — or even player-scoped —
  connection. Verified live, not inferred. A trigger is the only mechanism
  that reaches it. Migration 0073 closed this for
  `jurisdiction_resolution_active`.)*
- **No player-read policy on `jurisdiction_resolutions`.** This is a
  constraint on API design, not an oversight: the row records which basis
  was selected and which were rejected, so a player who can read it learns
  exactly which signal the platform trusted and which it ignored — directly
  attack-useful for steering a future resolution. Anything player-facing is
  a **curated server-side projection** ("your account is registered under
  jurisdiction X" at most), never a passthrough. Note the leading
  `app.player_account_id IS NULL` conjunct in each staff predicate is what
  makes this real: without it a player-scoped connection, which sets *both*
  GUCs, would satisfy a plain tenant match.
- **Staff read is its own permission**, not implied by `audit:read`,
  `bonus:read` or `player:read`; `considered_bases` is **projected out** for
  a caller lacking `verification:read`, because "a KYC basis was consulted /
  disagreed / was unavailable" are KYC-derived facts. *(Status: no staff
  read surface for `jurisdiction_resolutions` exists yet, so this is a
  binding constraint on whoever builds the first one — not a control in
  force.)*

### J4I.6 The platform registry, and a correction to the original ruling

> **Superseded by ADR 0046 (Stage 4I Phase E-SECURITY, migration 0077).**
> Everything below this point in J4I.6, as originally written, described a
> `jurisdictions`/`licences` posture that no longer exists. It is kept
> (struck through in spirit, corrected in fact) so the history of the
> finding is not lost, but it must not be read as the current state.
> `SEC-4I-F11`, which this section's own closing correction opened, is now
> **DISCHARGED** by migration 0077 — see the corrected text immediately
> following.

**Current state (migration 0077 / ADR 0046).** `tenants`, `licences` and
`jurisdictions` all now carry `ENABLE ROW LEVEL SECURITY` **and**
`FORCE ROW LEVEL SECURITY`. The live-reproduced defect that made this
necessary: `licence_country_ceilings_read` (migration 0076) anchors its
composite-ownership `EXISTS` on `tenants.licence_id`, and
`operating_country_policies_enforce_ceiling()` reads the same column, both
under the TENANT's own connection — and that connection could **write**
that column, letting one ordinary tenant-scoped transaction repoint its own
`tenants.licence_id` at another tenant's BYOL licence and commit a policy
for a country its own licence never permitted. Migration 0077 closes this.
The posture is deliberately asymmetric, not uniform:

- **`tenants` reads**: `USING (true)` for every non-player scope (Fix 5,
  this fix round, additionally excludes `app.player_account_id`-scoped
  connections, mirroring every sibling table). Kept open for non-player
  scopes because `identity.GetTenantBySlug` (staff login, no tenant context
  by construction) and several platform-wide `WithoutTenant` sweeps read
  `tenants` from scopes with no tenant match to offer — the same reasoning
  as the `assets` (migration 0044) precedent. This means a tenant-scoped
  connection can still enumerate every other tenant's non-licence-content
  columns (name/slug/licensing_model/status/licence_id) — recorded as a
  deferred, non-blocking item (`PLAT-TENANTREAD-1`) in the task registry,
  not a defect of this migration.
- **`tenants` writes**: platform-admin only. **No tenant-scoped write
  policy of any kind exists on `tenants`** — this is the actual fix for the
  defect above: a tenant can no longer write its own `licence_id`, full
  stop, independent of any application-level permission check.
- **`licences` reads**: narrowed to platform-admin, or the tenant whose own
  `tenants.licence_id` names the row — deliberately **not** `USING (true)`,
  because a BYOL tenant's own licence must not be readable by an unrelated
  tenant (ADR 0045 §4's ruling, applied one level down). Also excludes
  player scope.
- **`licences` writes**: platform-admin only, no tenant-scoped write policy.
  No DELETE policy (no production or test code deletes a licence).
- **`jurisdictions` reads**: `USING (true)` for every scope, including
  player — this is the one exception to Fix 5's player-exclusion pattern,
  kept deliberately: `jurisdictions` is public regulatory reference data,
  and the HDR-J-5 player-jurisdiction path needs it open to every scope.
- **`jurisdictions` writes**: platform-admin only. No DELETE policy.
- All three tables also now carry a `BEFORE TRUNCATE FOR EACH STATEMENT`
  deny trigger (Fix 4, this fix round) — RLS does not govern `TRUNCATE` at
  all, so the trigger is the only mechanism that reaches it, mirroring the
  pre-existing `licence_country_ceilings`/`operating_country_policies`/
  `platform_operations`/`audit_log` precedent.

The application-level permission check
(`PermJurisdictionRegistryManage`/`PermTenantLicenceAssign`, both
`RolePlatformAdmin`-only) **remains**, but as of migration 0077 it is no
longer the *only* control on the write side — `assertPlatformScope` in Go
and the platform-admin-only RLS policies enforce the identical predicate
independently, at the database, exactly as every other platform reference
table (`assets`, `asset_authorizations`) already does.

> **Correction (closing review) — historical record, now superseded.**
> §S-3.2 originally required `qa` to assert that "a tenant-scoped
> transaction can `SELECT` from `jurisdictions` and **cannot**
> `INSERT`/`UPDATE`/`DELETE`", citing `assets` as precedent, which was
> unachievable **at the time**: `jurisdictions`/`licences` then carried no
> RLS at all (unlike `assets`, which genuinely carried `ENABLE`+`FORCE`
> RLS), and a tenant-scoped transaction **could** `INSERT` into
> `jurisdictions`. That gap is exactly what migration 0077 closes: the
> assertion §S-3.2 originally wanted is now true and tested
> (`internal/jurisdiction/registry_rls_integration_test.go`,
> `internal/jurisdiction/qa_adversarial_registry_rls_integration_test.go`).
> "The permission check is the entire control on the write side; there is
> no database backstop behind it" is **no longer an accurate description of
> current state** — it described the pre-migration-0077 posture only.

### J4I.7 The three-layer non-forgeability mechanism

A jurisdiction value is exactly one of: **(a)** a scope declaration on a
configuration row — staff-authored by design, legitimately in a request
body, controlled by RBAC + FK + audit; or **(b)** a resolved fact about an
operation — **server-resolved only**. **No request body, header, query
parameter or path segment, staff- or player-supplied, may ever carry a
class (b) value.**

- **Layer 1 — no parse path.** The field does not exist on any
  enforcement-facing request struct, so `decodeJSON`'s
  `DisallowUnknownFields` turns a submitted value into a **400**, not a
  silent ignore. Removal, never keep-and-cross-validate: cross-validation
  manufactures a third state (resolver-unavailable-but-field-present) whose
  "just trust what the operator typed" resolution is a one-line change
  under incident pressure with no tripwire.
- **Layer 2 — two independent scope assertions, and a consumer implementing
  only one has implemented neither half usefully.** The **resolver** asserts
  the transaction's own `app.tenant_id` GUC against the tenant it is
  resolving for; the **consumer** asserts the resolution's binding against
  its own authenticated context before calling `Code()`/`ID()`. *(Corrected
  in the closing review: the resolver-side half was originally absent.
  **Further corrected by migration 0077/ADR 0046 (this fix round, Fix 10):**
  at the time of the original correction, this resolver-side assertion was
  the *only* thing standing between a transaction scoped to tenant B and a
  fully `Resolved` answer belonging to tenant A, because `tenants`,
  `licences` and `jurisdictions` then carried no RLS at all. That is no
  longer the whole story. Migration 0077 gave `licences` a narrow read
  policy (platform-admin, or the tenant whose own `tenants.licence_id`
  names the row), so tenant B's connection can no longer even **read**
  tenant A's licence row — `resolveTenantLicence`'s query would return zero
  rows and the function would return `refused(dependency_unavailable)`, not
  a `Resolved` result for tenant A, even without this assertion. The
  assertion remains necessary and load-bearing: without it, a mis-scoped
  call degrades only to a misleading fail-closed
  "dependency_unavailable"/"licensing unknown" result instead of a
  diagnosable scope-mismatch error — but it is now backed by a second,
  independent layer (the narrow `licences_read` RLS policy) that would also
  prevent the described cross-tenant leak even if the assertion were
  somehow bypassed. `tenants_read` remains `USING (true)` for non-player
  scopes (Fix 5 only excludes player scope), so the read-side gap on
  `tenants` itself is unchanged by migration 0077 — see J4I.6. Where a
  consumer derives both sides from the same values its half is
  defence-in-depth against future refactors — valuable, but not
  isolation.)*
- **Layer 3 — a non-forgeable value type.** `Resolution`'s fields are
  unexported, there is no exported constructor and no setter, and
  `Code()`/`ID()` return an error for any outcome other than `resolved`. No
  caller can obtain `""` or `uuid.Nil` from a resolution and proceed as
  though it had a real value.

**Where staff legitimately need to influence jurisdiction**, the channel is
correcting the underlying identity **fact**, through its own audited,
reason-coded surface — never a per-call override on an enforcement path.
Conflating the two is the actual design mistake to avoid.

### J4I.8 The nine-scenario isolation contract

Binding on `qa` for any jurisdiction-consuming surface. "Never data"
throughout means: never another tenant's/brand's/player's row content,
never a partial field, never an error message that discloses existence.

| # | Scenario | Required behaviour | Must NOT happen |
|---|---|---|---|
| 1 | **Cross-tenant** — tenant B's *valid* staff token requests tenant A's resolution | RLS returns zero rows; the handler surfaces **404**. A tenant-A resolution is also **unusable** as an input to any tenant-B operation even if an id were guessed | 200 with data; an error whose text distinguishes "exists but forbidden" from "does not exist"; a resolution crossing a tenant boundary in-process |
| 2 | **Cross-brand**, same tenant | Composite FK makes cross-tenant brand attachment impossible; within a tenant a brand-mismatched resolution is **refused, not silently widened**. A resolution with no brand is a tenant-level fact usable by any brand in that tenant | A brand-A resolution accepted for a brand-B operation because the tenant matched |
| 3 | **Player-scope read** | **Zero rows.** No player-read policy exists | A "my account" endpoint passing the row through instead of projecting it |
| 4 | **Forged jurisdiction payload** | **400** from `DisallowUnknownFields`; the server-resolved value is used regardless | The supplied value reaching any gate parameter, risk request, eligibility argument or persisted snapshot |
| 5 | **Forged tenant payload** | Tenant from authenticated context only; the resolver performs the same `app.tenant_id` assertion `internal/assetregistry` does | A resolution produced for a tenant the caller never proved it was |
| 6 | **Forged brand payload** | Brand derived server-side from the player account row; a body brand on an enforcement path is refused | A body brand narrowing or widening an authorization answer or a brand-scoped rule match |
| 7 | **Stale context** | Every resolution carries `as_of` and `resolver_policy_version`; an enforcement point that *reuses* a resolution rejects one older than its operation class's configured maximum age and **fails closed**. A *frozen* per-round snapshot and a *stale* resolution are different things and must be distinguishable in the record | An unbounded-age resolution reused indefinitely. *(Which jurisdiction **governs** across a change is a Human Decision Register item and is not asserted here.)* |
| 8 | **Conflicting sources** | Deterministic precedence from configuration; the disagreement is recorded as a per-basis **status**, values omitted; where precedence does not determine an answer the outcome is **`unresolved`**, never an arbitrary pick | A gate re-resolving or overriding; a silent "prefer the more permissive"; a silent "prefer the most recently written" |
| 9 | **Unavailable resolver** | `refused`; the operation **fails closed** with a distinguishable *internal* reason code; `audit_log` receives an **aggregated** entry; the **player-facing** response does not distinguish it from "blocked" | Falling back to a previously-known-good answer, a cached value, a tenant default or a brand default; one `audit_log` row per failed request |

> **Status of scenario 7 as of Stage 4I's close.** No staleness bound is
> implemented anywhere, and `Resolution.AsOf()` has no consumer. That is
> **correct today and only today**, for a precise reason: no resolution is
> ever cached or reused. Casino resolves inside the same transaction as the
> gate chain it feeds; Bonus resolves inside the gate transaction. Every
> resolution is milliseconds old and used exactly once. The obligation in
> row 7 becomes live the moment *anything* reuses a resolution across
> transactions — a cache, `AR-2`'s FK being read back, or a session-scoped
> reuse — and it must land in that same change, not after it.

### J4I.9 The adversarial test contract

Mandatory for any phase that touches jurisdiction resolution. Each case
names what it targets so `qa` is not starting from "test forgery." A `✔`
marks a case with executing coverage in the repository as of Stage 4I's
close; a `—` marks a case that is **specified and not yet covered**, which
is a disclosure, not a claim.

**A. Payload forgery — the value arrives from the wire**

- **A-1 ✔** Every routed enforcement surface, with a `jurisdiction_code` in
  the body, returns **400**. *One case per routed surface, not per struct*
  — routing, not the struct, is what a regression breaks.
- **A-2 —** The same surfaces with the field absent: the operation proceeds
  using the **server-resolved** jurisdiction, and the persisted snapshot
  matches the resolver's output, not any value the test could have
  supplied. (A-1 without A-2 would pass against a handler that ignores
  jurisdiction entirely.) *Not currently assertable end-to-end, because
  every player-scoped resolution is unresolved and every consuming gate
  therefore denies; it becomes assertable with HDR-J-3.*
- **A-3 —** A jurisdiction value supplied as an HTTP **header**
  (`X-Jurisdiction`, `X-Forwarded-Country`, `CF-IPCountry`), a **query
  parameter**, and a **path segment**: all ignored, resolved value
  unchanged. Headers are the case `DisallowUnknownFields` does **not**
  cover, and `CF-IPCountry`-shaped headers are exactly what a future geo
  implementation would be tempted to trust.
- **A-4 —** A jurisdiction value nested inside an otherwise-legitimate JSON
  field (a `reason_code` or `metadata` blob) never reaches any gate.
- **A-5 ✔ (structurally)** No path exists from a request body to the
  casino launch path's jurisdiction: the caller-supplied field was
  **deleted from the params struct**, so the property holds by compilation
  rather than by assertion.

**B. Context forgery — the value arrives from a manipulated identity**

- **B-1 ✔** A *valid* staff token for tenant B cannot cause a resolution
  for tenant A: refused, never data.
- **B-2 ✔** A resolver invoked on a transaction with **no** `app.tenant_id`
  **errors** — it does not return `unresolved`. These are different
  outcomes with different remediations, and reporting a connection-setup
  mistake as "no signal on file" is a silent-wrong-answer bug that becomes
  a fail-closed denial with a misleading cause.
- **B-3 ✔** A resolver invoked with a tenant argument disagreeing with
  `app.tenant_id` refuses **before** reading the other tenant's registry
  relationship, not merely eventually.
- **B-4 —** A player token for player X cannot produce, read or consume a
  resolution for player Y, same tenant **and** cross-tenant.
- **B-5 ✔** A platform-scoped principal (nil tenant, ADR 0011) cannot
  resolve at all — the attempt fails loudly rather than resolving against
  an empty read.

**C. Temporal — racing a change**

All of **C-1 … C-5 —** (configuration-boundary reuse, racing a
configuration write, a session predating a fact change, a frozen per-round
snapshot not being confused with a stale resolution, and the stuck-round
hazard) are **specified and uncovered**, for the same reason as scenario 7:
nothing reuses a resolution yet, and no player-side fact exists to change.
They are the first tests owed by whichever phase introduces reuse.

**D. Provenance forgery — the value is fabricated in-process**

- **D-1 ✔ (structurally)** A resolution cannot be constructed outside the
  resolver package: unexported fields, no exported constructor, no function
  anywhere that accepts a jurisdiction code from a caller and returns a
  resolution carrying it.
- **D-2 ✔** A resolution produced for one (tenant, player, brand) is
  refused when presented to a gate running for a different one.
- **D-3 ✔** No fallback exists: a sweep with no resolvable player
  jurisdiction **denies**. A fallback to a tenant or platform default is a
  Human Decision Register item, not an engineering decision, and is
  additionally forbidden by a database `CHECK`.
- **D-4 ✔** An empty code and an *unknown* code must never collapse to the
  same value, because a gate that treats the collapsed value as "no rule is
  scoped here" turns an invalid code into an ALLOW. Closed structurally —
  the translating helper was **deleted, not wrapped**, and no caller-supplied
  code exists to translate.

**E. Oracle / feedback channels**

- **E-1 ✔** The player-facing response for "jurisdiction unresolved" is
  **byte-identical** to the one for "jurisdiction blocked" — same status,
  same message, same body. Asserted on the serialised response, not on the
  sentinel. Internal distinctness is preserved in full; the collapse happens
  at the HTTP boundary only.
- **E-2 —** The two are not distinguishable by **timing** either, to within
  a coarse threshold. A resolver that short-circuits on one and scans on the
  other leaks the distinction through latency even with identical bodies.
  This is a coarse assertion, not a constant-time-crypto requirement.
- **E-3 / E-4 —** No player-facing or staff-facing read surface for
  `jurisdiction_resolutions` exists yet, so these are binding constraints on
  whoever builds the first one rather than presently-testable properties.

**F. Availability and fail-closed behaviour**

- **F-1 ✔** Resolver unavailable ⇒ the operation denies. It does not fall
  back to a previously-known-good answer, a cached value, a tenant default
  or a brand default.
- **F-2 —** N failed resolutions in one bucket produce **exactly one**
  `audit_log` entry (J4I.4's amplification guard). Uncovered because the
  event is unimplemented.
- **F-3 ✔** Two concurrent operations on the **same player**, with the
  resolver in the picture, complete without a deadlock.
- **F-4 —** The resolver holds no advisory lock and no row lock, and
  succeeds inside a `BEGIN … READ ONLY` transaction. **Partially covered:**
  the strongest of the three read-only enforcement mechanisms — the
  compile-time one, where the resolver accepts a `Query`/`QueryRow`-only
  interface so a write inside its body would not compile — genuinely holds.
  The other two do not exist (`DR-4I-QA-01`), and the `pg_locks` one exists
  specifically to catch a `SELECT … FOR SHARE` that the other two both
  permit.

**G. Isolation and RLS**

- **G-1 ✔** Every row of J4I.8 has at least one test, within the limits
  disclosed above for rows 7–9.
- **G-2 ✔** `DELETE` removes zero rows; `UPDATE` raises; `TRUNCATE` raises —
  on **both** tables (the second table's TRUNCATE case is SEC-4I-F8).
- **G-3 ✔** A resolver query on a bare pool connection is reported as an
  **error**, not as `unresolved`.
- **G-4 ✔ (restated)** A tenant-scoped transaction can `SELECT` from
  `jurisdictions`. The original "and cannot write it" half is withdrawn as
  unachievable — see J4I.6.
- **G-5 ✔** A tenant-scoped role cannot manage the platform game catalogue —
  a regression guard that matters more once a per-game jurisdiction
  blocklist is a live denial control.
- **G-6 —** The authoring-time precondition that would make a
  jurisdiction-scoped rule un-authorable for a `(tenant, operation)` pair
  not recorded as resolution-active is unimplemented (`DR-4I-RISK-01`), so
  its assertion is owed with it.

### J4I.10 Findings register — Stage 4I `security`

| Id | Finding | Severity | Status |
|---|---|---|---|
| `SEC-4I-F1` | A client-supplied approvals count flowed unclamped into four-eyes consumption, letting a caller lower a tenant's configured N-approver threshold to 1 on a path that releases real money | HIGH | **CLOSED** — field removed; resolved server-side from policy |
| `SEC-4I-F2` | Interim `staff_supplied` provenance label on staff-typed jurisdiction values | — (interim control) | **CLOSED** — superseded, not merely retired: the field it labelled no longer exists on any surface, and no site emits the label. The enum value is retained only so records written before removal remain distinguishable |
| `SEC-4I-F3` | The catalogue upsert's audit record carried no before/after, so a change to a jurisdiction blocklist left no trace of what it had been — a hard prerequisite for making that blocklist a live denial control | HIGH | **CLOSED** |
| `SEC-4I-F4` | `FOR ALL` policy on `jurisdiction_resolution_active` silently granted DELETE to every tenant-scoped transaction — an unaudited way to change a control's state | MEDIUM | **CLOSED** — per-command policies, no DELETE policy (migration 0072) |
| `SEC-4I-F5` | Resolution-active changes recorded *what* changed but never *why* | LOW | **CLOSED** — reason code required and persisted |
| `SEC-4I-F6` | A hand-copy of the manual-grant four-eyes logic in a test meant the production control was not what the test exercised | MEDIUM | **CLOSED** — single shared post-gate hook; test de-tautologised |
| `SEC-4I-F7` | Same shape on the grant-activation path | MEDIUM | **CLOSED** — unexported function-typed seam, production always binds the real implementation |
| `SEC-4I-F8` | No `BEFORE TRUNCATE` trigger on `jurisdiction_resolution_active`. RLS does not cover TRUNCATE, so every control on the table — including F4's own fix — was bypassable by one statement from any tenant- or player-scoped connection, erasing every tenant's control state unaudited | MEDIUM | **CLOSED** — migration 0073 |
| `SEC-4I-F9` | A per-game jurisdiction blocklist entry is compared to a resolved code by **exact, case-sensitive string match**, with no validation that the entry is a real `jurisdictions.code`. A blocklist of `["mt"]` against a registry code of `MT` leaves the game *armed* but the specific block silently inert. Registry codes are also not case-normalised, so `MT` and `mt` can both exist as distinct jurisdictions | MEDIUM when reachable; **inert today** (an armed game denies every launch while all player-scoped resolutions are unresolved, so the mismatch cannot manifest) | **OPEN** — routed to `casino` + `architect`. Trigger: HDR-J-3 |
| `SEC-4I-F10` | The resolver's licence lookup does **not** filter on `licences.status`. A suspended or expired licence would still yield `resolved` with `authoritative` confidence — a compliance fail-open. Currently unreachable: no status-transition operation is exposed and the column can only hold its default | LOW today; MEDIUM once reachable | **OPEN** — routed to `backend` + `architect`. **Hard trigger: any change introducing a licence status transition must land with a resolver-query change and a security review, in the same change** |
| `SEC-4I-F11` | Platform reference tables (`jurisdictions`, `licences`, `casino_games`) have no database-level write control at all; the HTTP permission check is the entire control. Pre-existing platform posture, not a Stage 4I regression, but materially weaker than every tenant-owned table | LOW | **OPEN** — routed to `architect` as a cross-cutting posture question |

### J4I.11 Scope of this review

**In scope:** `internal/jurisdiction` in full; migrations 0071–0073; the
jurisdiction admin HTTP surface and its route wiring; the two new
permissions and the complete role-permission map; the consuming gates in
`internal/casino` and `internal/bonus` **as jurisdiction consumers**; the
RLS posture of both new tables, re-tested adversarially against a live
database in their final state; and the audit/PII content of everything
Stage 4I writes.

**Explicitly NOT in scope:** `internal/risk`, `internal/rg`, `internal/kyc`
and `internal/payments` beyond their jurisdiction touchpoints; the Bonus,
casino, ledger and EOI surfaces as domains; sportsbook; retail; any UI; any
real external provider. **No claim is made that any surface is "secure" in
general**, and passing this review once does not make a feature secure
later. This is a code-level and design-level review appropriate to a
development-stage platform — **not** a penetration test and **not** a
certification-grade audit, both of which require external, human-run
engagements. No Human Decision Register item is decided here, and nothing
in this section authorizes a production launch.

## Stage 4I Phase B — player jurisdiction evidence foundation (declared/verified residence, location-signal abstraction)

Section numbering is `J4I.12.*`, continuing the Stage 4I numbering above.
This closes out the security model for the three evidence subsystems
authorized by the Stage 4I "Phase B" human directive (per HDR-J-3a/b/c/e/
f/g/h, `docs/decisions/0042-human-decision-response.md`): declared
residence, KYC-verified residence, and a physical-location signal
*abstraction* (interface + mock only, no vendor, no HTTP surface).

### J4I.12.1 New permissions

Two new permissions, both `RoleCompliance`-only, added to
`internal/auth/permission.go`'s `rolePermissions` map and verified by an
exhaustive-role test (`internal/auth/jurisdiction_evidence_permission_test.go`,
iterates `allRoles`):

- `jurisdiction_evidence_collection:activate` — the activation-boundary
  switch (§J4I.12.2 below). Deliberately **not** granted to
  `RoleTenantAdmin`, unlike the structurally similar
  `jurisdiction_resolution_active:write` — switching on collection of
  privacy-sensitive personal data is a lawful-basis judgment (HDR-J-3e),
  not a commercial/engineering configuration act.
- `player_residence:read` — would gate a staff-facing read of a
  residence *value* for a player other than the caller. **Defined and
  role-scoped but currently wired to zero HTTP handlers** — no
  staff-facing read surface exists in this phase. This is deliberate,
  not an oversight: the two read accessors this permission would
  eventually gate (`identity.GetDeclaredResidence`,
  `kyc.GetVerifiedResidence`) are themselves built standalone, with zero
  non-test callers, for a future phase to wire in. Recorded here so a
  future reader does not mistake the grant for a live capability.

No player-facing permission gates `GET`/`PUT /v1/me/residence` — a player
may always read/write their *own* declared residence subject only to the
activation boundary below, exactly like `GET`/`PUT /v1/me` and its
siblings.

### J4I.12.2 The activation boundary (two layers)

Building the technical capability to collect a signal is not the same
thing as having lawful basis to collect it (CLAUDE.md's Compliance
section), and HDR-J-3e requires this distinction be enforced, not just
documented. Two independent layers:

1. **`internal/jurisdiction/resolver.go` has zero diff in this phase.**
   `Resolve` does not read either residence column and does not call
   either new read accessor. So even a tenant with collection switched
   on and real data recorded has no path by which that data changes any
   jurisdiction-gated behavior yet — verified by `git diff --stat` on
   the file (empty) and by the accessors having no non-test callers.
2. **Per-tenant, per-evidence-type collection switch**
   (`jurisdiction_evidence_collection_active`, migration 0074;
   `internal/jurisdiction/evidence_collection_active.go`). Absent row is
   fail-closed (collection OFF by default for every tenant, every
   evidence type — verified live: `IsEvidenceCollectionActive` returns
   `false, nil` on `pgx.ErrNoRows`). Both write paths — `PUT
   /v1/me/residence` (declared) and the `verified_residence_country`
   field on the existing `POST /v1/admin/kyc/verifications/{id}/review`
   (verified) — check this flag **inside the same database transaction
   as the write** and roll back the entire call, including any
   unrelated status transition requested in the same call, when
   collection is off. Verified both by code inspection and by dedicated
   tests on both paths (`TestReviewVerification_ActivationGateOff_
   WholeCallFails` reads verification `status` before/after and asserts
   equality, not just that no residence value was set).

**Former named exception — enforcement asymmetry between the two write
paths — CLOSED by the PHASE-B-ARCH-1 hardening gate.** The KYC path's
gate was always inside `ReviewVerification` itself — unreachable-around
by any caller. The declared-residence path's gate was originally enforced
**only** in the HTTP handler (`newSetMyResidenceHandler`); the underlying
`identity.SetPlayerAccountDeclaredResidence` was an exported, ungated
function whose only protection was a doc comment instructing callers to
check the switch first — enforcement by discipline in application code,
the same pattern CLAUDE.md rejects for tenant isolation, applied here to
a lawful-basis gate.

A dedicated hardening-gate dispatch (`PHASE-B-ARCH-1`) closed this: the
check now lives INSIDE `SetPlayerAccountDeclaredResidence` itself, in the
same transaction as the write, mirroring `ReviewVerification`'s own gate
exactly, so both write paths now share the identical enforcement shape —
neither can be reached, by any Go caller, HTTP or internal, without the
check running first (this is an application-level control; it does not
stop a hand-written SQL statement issued outside these two functions —
no claim beyond that is made). The HTTP handler's own duplicate check was
removed rather than kept as defence-in-depth (the same predicate checked
twice by the same package graph is how the two paths drifted apart
originally).

A `BEFORE INSERT OR UPDATE` trigger — the remedy this section previously
recorded as "preferred" — was considered and rejected by the architect
ruling for this gate: the trigger would need to read `jurisdiction_
evidence_collection_active`, whose RLS policies exclude a player-scoped
connection, and the application role is `FORCE ROW LEVEL SECURITY`/
`NOBYPASSRLS`-bound, so the trigger would either misfire on legitimate
player-scoped writes or require a `SECURITY DEFINER` RLS-bypassing
function. **Correction (PHASE-B-ARCH-1's own independent security
review):** the "misfire on player-scoped writes" half of that argument is
now weaker than recorded — the fix above ADDS a connection-scope
assertion to `SetPlayerAccountDeclaredResidence` that guarantees a
residence write is never player-scoped, so a trigger written with a
`WHEN (NEW.declared_residence_country IS DISTINCT FROM OLD.
declared_residence_country)` guard would in fact never fire under player
scope, and would read the gate table fine under tenant scope. The
`SECURITY DEFINER`/hot-path arguments are unaffected and still hold. This
correction does not reopen the trigger decision on its own — the
architect's ruling stands, and no specialist unilaterally overturns
another's ruling — but the "revisit if a second writer of `declared_
residence_country` ever appears" condition that ruling already recorded
should be judged against this corrected reasoning, not the original.

**Named, accepted gap — TOCTOU on the gate read (both write paths,
pre-existing on KYC, not introduced by this hardening pass).**
`jurisdiction.IsEvidenceCollectionActive` is a plain `SELECT` with no row
lock, under this codebase's standard READ COMMITTED isolation. A write
already in flight when the switch is toggled OFF can still commit: the
write and its audit record land together in one transaction (data stays
internally consistent), but `audit_log` can then show a `player.declared_
residence_set` (or `kyc.verified_residence_determined`) entry timestamped
*after* the `jurisdiction_evidence_collection_active.changed` entry that
turned collection off for that tenant — the artifact a regulator/DPA
inquiry would notice first. Bounded to one in-flight transaction
(milliseconds); identical on both write paths; `kyc.ReviewVerification`
has carried this exact shape since Phase B shipped. Not fixed in this
pass — a fix (e.g. `SELECT ... FOR SHARE` on the gate row in a
write-path-specific accessor variant) is a cross-path design change
belonging to `architect`, not a hardening-gate-scoped patch, and is
recorded here as a named prerequisite for whichever future change next
touches either gate-check call site.

Full ruling, implementation, and independent review disposition:
`docs/governance/task-registry.md`'s "Stage 4I PHASE-B-ARCH-1" section.

### J4I.12.3 Audit content — what changed from the original design, and why

The general Stage 4I audit rules (§J4I.2–§J4I.4 above) apply unchanged:
never the evidence *value*, only the decision/fact. Two new event
families: `player.declared_residence_set` (identity-owned) and
`kyc.verified_residence_determined` (kyc-owned, written only when a
review call includes a determination, alongside the pre-existing
`kyc.verification_status_changed`).

**A real defect was found and fixed in this phase, not merely
theorized.** The initial `kyc.verified_residence_determined` metadata
included the reviewer's free-text `reason` field verbatim — the same
field already recorded on the sibling `kyc.verification_status_changed`
entry. Three independent reviewers (`security`, `architect`, and `qa`,
working from different angles: an adversarial read of the metadata map,
a fidelity check against the ruling's own "never record the value"
requirement, and a test-coverage gap hunt, respectively) converged on
the same finding. A reviewer's free-text reason is exactly the kind of
uncontrolled string that can carry the country a residence-determination
record exists to keep out (e.g. "resident of ES per utility bill") —
defeating the activation-gate mechanism in substance even when the
schema-level protections hold. **Fixed**: the `reason` key was removed
from this specific metadata map (it remains, appropriately, on the
sibling status-changed entry). A new test,
`TestReviewVerification_VerifiedResidenceDeterminedAuditRecordShape`,
deliberately puts the country in the reviewer's free-text reason and
asserts the raw audit row for `kyc.verified_residence_determined` never
contains it — mirroring `internal/identity`'s pre-existing
`TestSetPlayerAccountDeclaredResidence_AuditRecordShape` pattern, which
did not have an equivalent on the KYC side before this fix.

Both new audit-writing call sites in `ReviewVerification` now also carry
`IPAddress`/`UserAgent`/`RequestID` (previously absent on this package's
audit entries entirely, including the pre-existing status-changed entry)
— CLAUDE.md's audit rule names IP explicitly for every mutating
administrative action, and a staff act against another person's data is
exactly that.

### J4I.12.4 RLS — `jurisdiction_evidence_collection_active`

Built with its final hardened shape from day one (migration 0074), not
reproducing either of the two historical defects
`jurisdiction_resolution_active` shipped with and needed migrations
0072/0073 to fix: per-command SELECT/INSERT/UPDATE policies (no `FOR
ALL`, no DELETE policy), `FORCE ROW LEVEL SECURITY`, and a `BEFORE
TRUNCATE` deny trigger present from creation. Verified live against the
dev database (`pg_policies`, `pg_trigger`), not just against the
migration source text. `player_accounts`/`kyc_verifications`' own
pre-existing tenant-isolation policies are untouched by this migration
(it adds only columns and CHECK/FK constraints to those two tables).

### J4I.12.5 Findings register — Stage 4I Phase B (`security`, `architect`, `qa`)

See `docs/governance/task-registry.md`'s "Stage 4I Phase B" section for
the full disposition table (fixed/deferred, with owner and reason for
every deferral). Verdicts: `security` — CERTIFIED WITH NAMED EXCEPTIONS
(one P1, fixed in this phase's own fix round; the enforcement-asymmetry
exception at §J4I.12.2 remains open pending an `architect`+`security`
joint disposition, gating `PHASE-B-ARCH-1` only, not this phase).
`architect` — CERTIFIED WITH NAMED EXCEPTIONS (no blocking issues; nine
design rulings all conformed, `resolver.go` and the `Basis` enum
verified at zero diff). `qa` — READY WITH NAMED GAPS (three P1 test-
coverage gaps, all closed in this phase's own fix round).

### J4I.12.6 Scope of this review

**In scope:** `internal/identity`'s declared-residence addition,
`internal/kyc`'s verified-residence addition, the new
`jurisdiction_evidence_collection_active` table and its admin surface,
the two new permissions and their role grants, `internal/geolocation` (a
drive-by read by `security` even though it was not in the original
review brief, since it is a new privacy-adjacent package in the same
working tree). **Explicitly NOT in scope:** any real physical-location
vendor (none exists — `internal/geolocation` has no vendor, no route, no
registration); HDR-J-2's precedence policy (not implemented); production
activation of any of this phase's collection switches; penetration
testing or certification-grade audit. No claim is made that this
subsystem is "secure" once and for all — the moment `player_residence:
read` gets an actual endpoint, or the two read accessors are wired into
`resolver.go`, that is a new review, because the value-exposure surface
changes completely at that point.

## Stage 9 — production readiness: session/auth, RBAC audit, API resilience

`security`-owned record for Stage 9 §7 (authentication/session), §8
(authorization/RBAC audit) and §21 (API security/resilience). Scope of
this review is stated explicitly at the end — nothing here says the
platform "is secure".

### S9.1 Per-IP rate limiting on the unauthenticated credential surface

`internal/httpserver/ratelimit.go` adds a single-process, in-memory,
fixed-window per-IP limiter in front of the endpoints that are reachable
without any credential: `POST /v1/auth/register`, `/v1/auth/login`,
`/v1/staff/auth/login`, `/v1/auth/refresh`,
`/v1/auth/password-reset/request`, `/v1/auth/password-reset/confirm`,
`/v1/auth/email-verification/confirm`.

**The gap it closes** is specific, not a generic "APIs should have rate
limits" gesture:

1. `internal/identity/login_attempt.go`'s lockout is per-IDENTIFIER
   (brand+email / tenant+email): five failures against ONE account in
   fifteen minutes. It does nothing against one password tried against
   ten thousand accounts, which is what a credential-stuffing run
   actually is. Nothing bounded that before.
2. `auth.HashPassword`/`VerifyPassword` deliberately cost ~64 MiB and
   ~100 ms each, and login pays that cost even for an email with no
   account (`auth.DummyPasswordHash`'s constant-time branch). Unbounded,
   the platform's own password-hardening parameters become a remote
   memory-exhaustion lever usable with no credentials at all.
3. `/v1/auth/password-reset/request` was per-ACCOUNT limited
   (`auth.CountRecentCredentialTokens`) but not per-caller, so one caller
   could still walk an address list; both `confirm` endpoints had no
   per-caller bound of any kind.

**What it deliberately is NOT.** It is not distributed and not an
edge/WAF control, and must not be described as either:

- With more than one `platform-api` replica each replica enforces its own
  window, so the platform-wide effective limit is (limit × replicas).
  Still a bound; not the configured number.
- It keys on `clientIP(r)`, i.e. `RemoteAddr`. `X-Forwarded-For` is
  deliberately not trusted, because no specific reverse-proxy chain is
  configured yet (see `clientIP`'s own doc comment, and
  `TestRateLimit_IgnoresXForwardedFor`).
- `rateLimiterMaxKeys` fails OPEN, on purpose: an attacker reaching the
  service from ~100k distinct real source addresses already has a botnet
  this control was never going to stop, and denying every login in that
  situation would convert their resource exhaustion into a complete
  authentication outage for legitimate players. The property being
  protected at that boundary is bounded memory, not the rate limit.

**S9.1-LAUNCH-1 (launch gate, open).** The first deployment that puts a
load balancer or ingress in front of `platform-api` makes every request
arrive from one address, at which point this control degrades from "per
client" to "per service, globally", and the configured numbers become a
cap on total login throughput. Whoever introduces that proxy MUST do one
of: teach `clientIP` to trust that specific proxy chain's
`X-Forwarded-For`; raise/disable the limits via
`httpserver.Deps.AuthRateLimitPerMinute`; or move the control to the
edge. This must not be discovered in production.

**S9.1-LAUNCH-2 (launch gate, open — owner: `devops`).**
`Deps.AuthRateLimitPerMinute` exists precisely so S9.1-LAUNCH-1 can be
answered without a code change, but it is **not wired to
`internal/config` or `cmd/platform-api/main.go` yet**, so today it is
reachable only from a code change or a test. It needs an
`AUTH_RATE_LIMIT_PER_MINUTE` environment variable (0 = per-bucket
defaults, >0 = override every bucket, <0 = disable) plumbed through
`config.Config` into the `Deps` literal. Until that exists, the documented
escape hatch is not operationally available.

### S9.2 `RequirePlayerPrincipal` — the player self-service surface

`auth.RequirePlayerPrincipal` (`internal/auth/middleware.go`) now gates
every `/v1/me/**` and `/v1/bonus/**` player route. Those routes carry no
`RequirePermission` gate by design ("a player has an inherent right to
act on their own account") and instead derive the acting
`player_account_id` from the token subject.

Before Stage 9 that left the whole surface reachable by a STAFF bearer
token. Nothing was actually disclosed — every such handler feeds
`tc.Subject` into a `player_accounts` lookup or a player-scoped RLS GUC,
and a staff id matches no player account — but the safety was
INCIDENTAL, resting on "no staff id will ever collide with a player id"
and on every future handler remembering to do a lookup that happens to
fail. `CLAUDE.md`'s rule is that authorization is enforced server-side,
not inferred, so the principal type a route is written for is now
asserted once, at the route, rather than re-derived by accident in each
handler. Severity as found: **P3 (defense-in-depth / latent-P1)** — no
exploitable disclosure today, a P1 the moment a `/v1/me` handler is added
that trusts `tc.Subject` without a lookup.

**Named exception, asserted not assumed:** `GET /v1/me/sessions` and
`DELETE /v1/me/sessions/{id}` deliberately stay open to every principal
type. "Which devices am I logged in on, and log that one out" is a
self-service capability a staff user owns over its OWN sessions;
`auth.ListActiveSessions`/`auth.RevokeSession` both scope strictly to the
caller's `principal_type` + `principal_id`, and `sessions`' RLS
additionally requires `app.principal_id` to match. Pinned by
`TestSessionRoutes_RemainOpenToStaffPrincipals`.

### S9.3 Required authorization / tenant-isolation test coverage

Every domain specialist's `qa` coverage for a new endpoint must include,
as a minimum:

1. **Cross-tenant:** a request for tenant A's data carrying tenant B's
   VALID token returns 403/404 — never data, never a 500 that leaks
   existence.
2. **Principal-type:** a staff token on a player self-service route
   returns 403; a player token on an admin route returns 403.
3. **Permission:** a token holding a role WITHOUT the route's permission
   returns 403, even when the same role can reach neighbouring routes.
4. **Identifier ownership (IDOR):** an id belonging to another
   player/tenant, presented in the path or body by an otherwise valid
   caller, returns 404 (not 403 — not leaking existence) and performs no
   write.
5. **Anonymous:** no bearer token returns 401, not 403 and not 500.

The Stage 9 additions of this shape live in
`internal/httpserver/player_surface_principal_test.go`,
`internal/httpserver/ratelimit_routes_test.go`,
`internal/httpserver/ratelimit_test.go` and
`internal/auth/require_player_principal_test.go`.

### S9.4 Verified, with no defect found

Reviewed at code level this stage and found correct — recorded so a later
reviewer knows these were actually looked at, not skipped:

- **Algorithm confusion / `alg: none`.** `Issuer.Verify` pins
  `jwt.WithValidMethods([]string{"HS256"})` AND re-checks
  `*jwt.SigningMethodHMAC` inside the keyfunc, requires `iss`, `aud` and
  a present `exp` (`jwt.WithExpirationRequired`), and rejects an unknown
  `kid` before any signature check.
- **Key rotation.** `auth.KeyRegistry` genuinely supports active +
  previous (`JWT_ACTIVE_KID`/`JWT_SIGNING_SECRET` +
  `JWT_PREVIOUS_KID`/`JWT_PREVIOUS_SECRET`), enforces a 32-character
  minimum on every key, and rejects a previous kid equal to the active
  one. Operationally, rotation is a restart-with-new-env-vars, and a
  token signed by a retired key stays valid only until its own `exp` —
  acceptable given the short access-token TTL, but there is no
  force-revoke-by-kid mechanism, and no written runbook. Noted, not
  blocking.
- **Refresh rotation and reuse detection.** Single-use rotation with a
  conditional `UPDATE ... WHERE replaced_by_session_id IS NULL AND
  revoked_at IS NULL` checked via `RowsAffected` (race-safe); presenting
  an already-rotated token revokes the entire chain AND writes an
  `auth.session_reuse_detected` audit record atomically with the
  revocation; `revokeChainFrom` continues past an already-revoked
  mid-chain node.
- **Hashed-at-rest tokens.** Refresh tokens and credential tokens are
  256-bit random values stored only as SHA-256 hashes; raw values are
  returned exactly once and never read back.
- **Argon2id parameters.** 64 MiB / t=1 / p=4 / 16-byte salt / 32-byte
  key, at or above OWASP's baseline, with parameters embedded in the
  stored hash so they can be raised without breaking existing hashes.
  Comparison is `subtle.ConstantTimeCompare`.
- **Lockout is enforced, not merely recorded.** Both
  `newLoginHandler` and `newStaffLoginHandler` call
  `identity.IsLockedOut` BEFORE any password comparison and refuse the
  request regardless of whether the presented password is correct. Both
  normalize the email before building the lockout identifier, so
  case/whitespace variants cannot each get their own bucket.
- **Request body limits.** `decodeJSON` caps at 1 MiB with
  `DisallowUnknownFields`; the three provider webhooks cap independently
  (`maxWebhookBodyBytes`, `maxCasinoWebhookBodyBytes` = 1 MiB,
  `maxKYCWebhookBodyBytes` = 256 KiB) and reject an oversized body rather
  than truncating it; document upload uses `http.MaxBytesReader`.
- **Pagination.** `parsePageParams` clamps `?limit=` to
  `maxPageLimit` (200) and treats malformed input as the default. No
  list endpoint accepts a client-controlled unbounded limit.
- **Malformed input / panic.** `recoverMiddleware` converts any handler
  panic into the standard 500 envelope with the stack logged server-side
  only; every `r.PathValue` id goes through `uuid.Parse` with a 400 on
  failure.
- **Replay protection** on financial callbacks already exists from Stage
  8 (`internal/idempotency`, whose `OccurrenceSource` contract refuses to
  derive an occurrence discriminator from any unsigned transport-level
  signal). Confirmed, not rebuilt.
- **Secrets.** No hardcoded credential and no secret value in any log
  call. `internal/email`'s package contract forbids logging a message
  body (which carries a raw verification/reset token) — a future real
  vendor adapter MUST NOT put the body into the error it returns, since
  `newRequestPasswordResetHandler` logs that error.

### S9.5 Scope of this review

**In scope:** `internal/auth` in full; `internal/httpserver`'s route
table, middleware chain, and the player self-service and admin handlers
spot-checked per domain; `internal/httpserver/ratelimit.go`;
`internal/identity`'s login-attempt lockout; the credential-token flows.
**Read-only, not modified:** `b2c/` and `backoffice/` auth modules.
**Explicitly NOT in scope and NOT claimed:** penetration testing,
certification-grade audit, any TLS/ingress/WAF configuration, the
production secrets backend (Vault/KMS — still only the design in
"Secrets" above, no implementation), and production launch authorization
of any kind.

**Known, documented, still open:** both B2C and Back Office store the
refresh token in `sessionStorage` and the access token in a module
variable (`b2c/src/auth/tokenStore.ts`,
`backoffice/src/auth/tokenStore.ts`). Both files already state the
tradeoff and the intended fix (server-set `httpOnly`, `Secure`,
`SameSite` cookies, which requires a backend change). This is materially
worse for a public B2C app than for internal staff and should be treated
as a pre-launch item, not an indefinite deferral.
