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
