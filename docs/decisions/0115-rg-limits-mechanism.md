# ADR 0115 — Responsible Gaming limits, time-out and jurisdiction-aware RG configuration

## Status

**PROPOSED** (architect, 2026-10-12). Design only; nothing here is implemented.
Owner directive 2026-10-11: finish provider-independent RG. No legal value is
chosen here. Every numeric limit, cool-down, period, duration and cadence is
**configuration per jurisdiction**, and the configuration has **no default that
implies legal compliance**. Compliance/legal supplies the content later (§9).
Migration numbers are allocated by the orchestrator only.

## Context

- Blueprint requirement (as extracted in `docs/architecture/01-requirements-inventory.md` §6,
  `docs/architecture/11-kyc-aml-rg-architecture.md` "Responsible gaming"): deposit, loss,
  wager and session limits. A decrease takes effect immediately and an increase only
  after a cooling-off period. Reality checks, time-outs, and self-exclusion at brand and
  platform level. A permanent self-exclusion survives account closure and
  re-registration (person cluster).
- Implemented today (`internal/rg/rg.go`, ADR 0026, migrations 0037/0038): one
  restriction type, `self_exclusion` (CHECK in `migrations/0037_create_player_restrictions.up.sql:49`).
  It is append-only, and immutability triggers enforce that. Player self-service
  exclusions apply platform-wide. Staff exclusions are tenant- or brand-scoped.
  `EvaluateEligibility` (`rg.go:584`) takes the L0.4 person advisory lock (`rg.go:138`)
  and checks the player account status, active self-exclusions (with `clock_timestamp()`),
  and the wallet status. Other parts already in place: the open-bet policy
  (`self_exclusion_policy.go`, ADR 0034 §14, which shows the effective-dated,
  tighten-only, no-backdating config pattern), the enumeration sweep
  (`enumeration_sweep.go`) and cross-brand person resolution (`internal/identityresolution`,
  ADR 0027, **MOCK** resolver only).
- Current callers of `EvaluateEligibility`:
  - casino launch: `internal/casino/orchestrator.go:399`
  - casino bet: `orchestrator.go:1647`, after L0.1 (`:1408`) and L0.2 (`:1630`), before
    risk (`:1688`) and L3 (`:1805`)
  - sportsbook place: `internal/sportsbook/orchestrator.go:279`, before L3 (`:483`)
  - deposit T1: `internal/payments/deposit_v2.go:207`
  - deposit T2: `internal/payments/drive.go:88`
  - bonus eligibility: `internal/bonus/eligibility.go:95`

  Rollback, win, void and settlement paths deliberately do **not** call it
  (`casino/orchestrator.go:1954,2096`).
- Gap audit `docs/governance/platform-completion-gap-audit-2026-10-10.md` §7 found no
  deposit, loss, wager or session limits, no time-out, and no cool-down on limit
  increases. Risk rules (`internal/risk`, ADR 0031) are **operator** limits. They are
  not player-set and have no cool-down semantics, so they are not reused as RG limits.
  Only the risk engine's ledger-usage measurement is reused (§3.4).
- Constraints:
  - ADR 0082 lock order: status keys, then L0.1..L0.6, then L1..L4, with rule R8.
  - Q-GP-5: terminal stake returns are allowed (task-registry
    `DECISIONS-PRH2-FINAL-GAMEPLAY-SECURITY-2026-10-06`).
  - WD-RG-1 is still open (`docs/HANDOVER.md:481`).
  - `tenant_jurisdiction_configs` exists but has no reader (gap audit §0.1). It is not
    reused because it holds opaque ruleset ids, not versioned, tighten-only values.

## Decision

### 1. Controls, scopes and periods

| Control | Kind key | Unit | Scope options (config) | Period |
|---|---|---|---|---|
| Deposit limit | `deposit` | asset minor units, `NUMERIC(38,0)` | `account` \| `tenant_person` | yes |
| Loss limit | `loss` | asset minor units | `account` \| `tenant_person` | yes |
| Wager limit | `wager` | asset minor units | `account` \| `tenant_person` | yes |
| Session duration limit | `session_duration` | seconds | `account` \| `tenant_person` | no (per login session) |
| Reality-check cadence | `reality_check_interval` | seconds | `account` | no. **Notification config only**; never blocks a bet |
| Time-out / cooling-off break | restriction `time_out` | duration | `platform_person` \| `tenant_person` \| `account` | n/a |
| Self-exclusion (exists) | restriction `self_exclusion` | duration or indefinite | `platform_person` (player); tenant/brand (staff) | n/a |

**Scope rules**

- Self-exclusion stays **person-level and platform-wide** for player self-service
  (ADR 0026 §3, unchanged). The same applies to `time_out` when the config says
  `platform_person`.
- Amount and session limits can be at most **`tenant_person`**, meaning all of one
  Person's accounts across the brands of one tenant. They are never cross-tenant:
  - usage comes from tenant-RLS-scoped ledger rows;
  - B2B tenants are separate operators with separate licences (ADR 0006).

  Cross-tenant amount limits are owner question OQ-3.
- `tenant_person` aggregation depends on Person linking. Person linking is
  **PROVIDER DEPENDENT** (MOCK resolver, ADR 0027), and the same caveat already
  disclosed for self-exclusion applies here.

**Periods**

- Allowed periods are `rolling_24h`, `rolling_7d`, `rolling_30d`, `calendar_day`,
  `calendar_week` and `calendar_month`.
- Calendar periods use the IANA timezone from the config row. Week start comes from
  config.
- Jurisdiction config lists which periods a player may choose.
- A player may hold several limits of one kind, for example daily and monthly. All of
  them apply.

**Assets**

- Amount limits are **per asset code**, keyed to the `Asset` registry exponent
  (ADR 0007/0021). There is **no FX conversion**.
- A jurisdiction marking a kind `mandatory` means every asset the player transacts in
  needs a limit, otherwise the action is refused (§3.2).

**Usage definitions (v1)**

- **wager** = sum of stake legs (cash, plus bonus-funded legs if
  `count_bonus_funded=true`) minus stake returns (rollback/void) in the window.
- **loss** = (stakes) − (wins + stake returns) in the window. Only the
  `net_of_wins` computation is supported. A config naming any other computation is
  rejected at write time, so an unsupported regime fails closed.
- **deposit** = gross sum of deposit intents in the window with status not in
  {declined, failed, expired}, including in-flight ones, using the credited amount
  once known. Reversals and withdrawals do not restore capacity.
- **session_duration** = now − the `created_at` of the root of the refresh-token
  rotation chain (`migrations/0012_create_sessions.up.sql`, `replaced_by_session_id`).

**Deferred (recorded, not built)**

- Cumulative play time per period: there is no reliable activity signal. It needs its
  own design.
- Operator-imposed (affordability) limits.

### 2. Data model

**2.1 `rg_limit_configs` (versioned config; pattern from migration 0043/0049 and `self_exclusion_policy.go`)**

- **Key:** `(jurisdiction_code, tenant_id NULL, brand_id NULL, control_kind, effective_from)`.
  - A row with `tenant_id NULL` is the **jurisdiction floor**. It is written by a
    platform principal only.
  - Tenant and brand rows are **operator bounds** and may only tighten the floor.
- **Fields.** Every field is **tri-state and explicit**: a value, the literal `none`
  (an attested "no bound"), or not allowed. A row cannot be stored with a field
  missing (DB `CHECK`).
  - `enabled` (bool): whether players may set this kind at all.
  - `mandatory` (bool)
  - `ceiling` (a value or `none`): the hard cap and the maximum a player may set.
  - `allowed_periods[]`
  - `calendar_tz`
  - `week_start`
  - `increase_cooldown` (an interval). There is no `none`; zero is rejected. Its
    absence makes increases impossible.
  - `increase_requires_reconfirmation` (bool)
  - `scope` (`account` | `tenant_person`)
  - `count_bonus_funded` (bool)
  - `loss_computation` (`net_of_wins`)
  - Time-out row: `allowed_durations[]` or `any`, and `scope`.
  - Reality-check row: `allowed_intervals[]`, `mandatory`.
  - `reason_code`, `attestation_ref` (the compliance/legal reference that supplied the
    value), `created_by_*`, `created_at`.
- **Append-only.** No UPDATE or DELETE, enforced by trigger like
  `player_restrictions_immutable`. The value in force is the latest
  `effective_from <= asOf` per exact scope.
- **Floor writes** may not be backdated (the ErrJurisdictionFloorBackdated rule and
  its 5 s tolerance).
- **Tighten-only** is checked twice: in Go, with an audited denial, and by a DB
  trigger, for operator rows. Tighter means:
  - lower ceiling;
  - longer cool-down;
  - `mandatory` false→true;
  - reconfirmation false→true;
  - `count_bonus_funded` false→true;
  - wider scope (account→tenant_person);
  - a subset of allowed periods and durations.
- **Read-time resolution** composes floor, tenant and brand rows by taking the
  strictest value per field, so it is safe even if a trigger is bypassed.
- **RLS:**
  - Read: floor rows are readable by all; tenant/brand rows by
    `tenant_id = app.tenant_id`.
  - Write: platform principal for floors; same-tenant for overrides
    (as with `sb_jurisdiction_restrictions`, migration 0090).
- **Jurisdiction key** is derived server-side from the tenant licence. This is the
  same derivation as `jurisdiction.EvaluationPolicyLookup`
  (`internal/jurisdiction/evaluation_policy.go`), and no request body can name it. A
  future per-player-jurisdiction overlay composes by taking the strictest value.

**2.2 `player_rg_limit_changes` (append-only history; the authoritative player-set state)**

- **Columns:**
  - `id`, `tenant_id NOT NULL`, `person_id`, `player_account_id` (anchor),
    `brand_id`
  - `scope`, `control_kind`, `asset_code NULL`, `period_kind NULL`
  - `value NUMERIC(38,0) NULL`, where NULL means removal
  - `change_kind` (`initial|decrease|increase|removal|cancel_pending|confirm`)
  - `requested_at` (DB `clock_timestamp()`), `effective_at`
  - `supersedes_change_id NULL` (for `confirm`/`cancel_pending`)
  - `source` (`player_self_service|staff_on_player_request`)
  - `reason_code`, `evidence_ref` (required for staff)
  - `created_by_actor_type/id`
  - `config_version_id` (provenance)
  - `idempotency_key`, with `UNIQUE (tenant_id, player_account_id, idempotency_key)`
- `change_kind` is **classified server-side** by comparing with the currently
  effective value. The client never asserts direction.
- **Timing rules:**
  - `initial`, `decrease` and `cancel_pending` take effect at `requested_at`
    (immediate).
  - `increase` and `removal` take effect at `requested_at + increase_cooldown`, read
    from the config **in force at request time**. If the cool-down is unconfigured,
    the request is refused (`rg_cooldown_unconfigured`), which leaves only the safe
    direction open.
- **Realization rule (deterministic, no UPDATE).** Order requests per key
  `(person or account, kind, asset, period)` by `requested_at`.
  - A row is *realized* iff `effective_at <= min(t, requested_at of the next request
    for the key)`. In other words, any newer request made before an increase matures
    supersedes that increase.
  - The effective player value at `t` is the latest realized row.
  - If `increase_requires_reconfirmation`, an `increase` row is never realized itself.
    A later `confirm` row (allowed only once `t >= increase.effective_at`, with no
    newer request) carries the value and is effective immediately.

  This is computed with one indexed query (`LEAD(requested_at)` window) on
  `(tenant_id, person_id, control_kind, asset_code, period_kind, requested_at DESC)`.
- **Effective limit** = min(player value or +∞, composed `ceiling`). Lowering a
  ceiling therefore clamps existing player limits immediately.
- If the kind is `mandatory`, there is no player value, and the ceiling is `none`, the
  action is refused with `rg_limit_required`. The platform never invents a value.
- **RLS:**
  - Tenant isolation on `tenant_id` (`WithTenant`).
  - Player self-read and self-insert via `app.player_account_id` (as in 0037's
    `player_self_*`), re-deriving `person_id`.
  - No platform-wide read.
- Every writer takes `rg.lockPerson` (L0.4) first.

**2.3 `player_restrictions`.** Extend the CHECK to
`('self_exclusion','time_out')` and keep the same table, locks, RLS and immutability.
`EvaluateEligibility`'s EXISTS query reads both types and returns distinct codes,
`self_excluded` and `timed_out`. A time-out cannot be ended early (append-only, as in
ADR 0026 §2). The same applies to the open-bet policy question, which stays scoped to
self-exclusion only.

**2.4 `casino_launch_sessions`.** Add `auth_session_root_started_at` (NOT NULL for new
rows), recorded at launch from the authenticated session chain. Bets recompute the
session deadline from the *current* limit, so a decrease applies mid-round.

### 3. Enforcement

**3.1 One boundary.** Add `rg.EvaluateAction(ctx, tx, ActionParams{TenantID, BrandID,
PlayerAccountID, WalletID, Action, AssetCode, Amount, CandidateRef, AuthSessionRootStartedAt})`.
- `Action` is one of `deposit|casino_launch|casino_wager|sportsbook_wager|session_refresh`.
- It calls `EvaluateEligibility` first, which keeps the L0.4 lock, the status checks,
  self-exclusion and time-out, and then:
  1. Config resolution. With no floor row in force, the result is
     `rg_config_missing` (§6).
  2. Session deadline.
  3. Amount limits: for each effective limit of the action's kinds,
     allowed iff `usage + amount <= limit`. Equality is allowed.
- It returns the existing `Decision` type with new codes: `timed_out`,
  `rg_config_missing`, `rg_limit_required`, `deposit_limit_reached`,
  `loss_limit_reached`, `wager_limit_reached` and `session_limit_reached`.
- ADR 0026 §5 "single boundary" is preserved. Callers swap `EvaluateEligibility` for
  `EvaluateAction` and keep their existing denial-audit helpers. Bonus eligibility
  keeps calling `EvaluateEligibility`, which now also covers time-out.

**3.2 Hook points (all already at the RG position; no call is moved)**

| Path | File:line | Kinds checked | Note |
|---|---|---|---|
| Deposit T1 `InitiateDepositAttempt` | `payments/deposit_v2.go:207` | deposit, mandatory-limit, config | `CandidateRef` = the just-inserted intent id, excluded from usage and added once as `Amount` |
| Deposit T2 `driveCreatedAttempt` | `payments/drive.go:88` | the same, re-checked | A decrease made between T1 and T2 declines an unsubmitted intent |
| Deposit receipt/success (`receipt.go`) | — | **none** | Money already taken is never refused. If the credited amount is above the limit, raise the alert `rg_deposit_limit_exceeded_post_facto` |
| Casino launch | `casino/orchestrator.go:399` | config, session, mandatory | The launch-token `expires_at` is capped at the session deadline (ADR 0103 contract unchanged otherwise) |
| Casino bet `postBet` | `casino/orchestrator.go:1647` | wager, loss, session | Covers bonus-funded legs per `count_bonus_funded`. Runs after the idempotency short-circuit, so replays are never re-evaluated |
| Sportsbook place | `sportsbook/orchestrator.go:279` | wager, loss (stake counts as potential loss), session | Bonus-funded sportsbook stakes remain blocked platform-wide (`sportsbook/orchestrator.go:120`) |
| Player refresh/login | `internal/auth` session rotate | session_duration | Past the deadline: refresh is refused and the player is logged out. Withdrawal still works after re-login (a new chain). The **session limit is per login chain**, by definition |
| Wins, casino rollback, sportsbook void/settlement/rollback, Q-GP-5 stake returns | `casino/orchestrator.go:1954,2096`, `sportsbook/settlement.go` | **none** | Never RG-gated. Returns reduce wager/loss usage automatically (ledger-derived) |
| Withdrawal request/approval/payout | `internal/withdrawal`, `payments/payout.go` | **none** | Withdrawals are never blocked by deposit/loss/wager limits, time-out or `rg_config_missing`. WD-RG-1 stays a separate owner item |
| Bonus claim/grant | `bonus/eligibility.go:95` | eligibility (self-exclusion + time-out) | A bonus grant is not a deposit. Bonus conversion is not counted as deposit |

**3.3 Locks and ordering (ADR 0082).** **No new hot-path lock class.**
- Every check above runs **inside the L0.4 person lock** that `EvaluateEligibility`
  already takes, at its existing position (after the status keys, L0.1 and L0.2, and
  before L0.5 risk, L1, L2 and L3, per R8).
- Usage, limit and config reads are plain MVCC reads using `clock_timestamp()`, not
  `now()` (the `rg.go:618` lesson).
- Because the limit writers and every gated action for a Person serialize on the same
  L0.4 key, two concurrent wagers on two brands for one person cannot both pass a
  `tenant_person` wager limit. The decrease-versus-bet race is decided by whoever
  takes the lock first.
- Config writers take a writer-only scope advisory (as in
  `lockOpenBetSelfExclusionPolicyScope`). It is never co-held with any L0.x, L1 or
  later lock, so it is outside the gameplay order. Record it as an ADR 0082 amendment
  note, not as a new class.
- Deposit T1 inserts its intent before the RG gate. That is a new row, not a lock on
  an existing row, so R8 holds as today.

**3.4 Usage source and cost.**
- **Wager and loss** come from `ledger_entries`, through a **read-only measurement
  function exported from `internal/risk`** that reuses `cumulativeSpec` and its
  undeclared-leg fail-closed check (`internal/risk/cumulative.go`, ADR 0031 §33). It
  is not a second, independent implementation. It uses index
  `idx_ledger_entries_account (ledger_account_id, created_at)` per account, with one
  range scan per (account, asset, window).
- **Deposit** comes from `deposit_intents` (payments-owned read helper). A new index
  on `(tenant_id, player_account_id, asset_code, created_at)` is needed.
- **Cost per gated action:**
  - one config read (indexed, in-tx, **not cached**, so a tightening is never served
    stale);
  - one limit-history read;
  - 0..N usage scans, where N = active limits for the action.
- Slice S3 must deliver EXPLAIN plans and a measured p95 delta on the casino bet path.
  No SLA is claimed. If 30-day rolling scans prove costly, daily rollups derived from
  the ledger can be added later, with drift-checking (no authoritative counters).
  Redis/cache is never consulted (CLAUDE.md).

**3.5 Fail-closed.**
- Any error while reading restrictions, config, limit history or usage returns an
  error. The caller's transaction aborts, so nothing is posted, no deposit intent
  proceeds and no session is minted (launch uses the existing `launchFailed` path).
- An undeclared ledger leg is an error, never "zero usage".
- Because the error path rolls back in-tx audit, the caller logs it out of the
  transaction and emits the durable alert `rg_evaluation_unavailable` (ADR 0102),
  as `launchFailed` does today.
- Protective player actions are **never** blocked by missing config: decrease, cancel
  of a pending increase, self-exclusion, and time-out (any positive duration when the
  allowed durations are unconfigured).

### 4. Audit and alerts

- **Audit records.** Every limit request is audited via `audit.Record` in the same
  transaction, whether accepted or refused. Each record includes:
  - action `rg.limit.change_requested|…_denied|confirmed|cancelled`;
  - actor, tenant, person, account, kind, asset, period;
  - before and after effective value, pending value and `effective_at`;
  - config version, reason code, IP, UA and request id.
- Time-out uses `rg.time_out.created`.
- Every enforcement denial is audited with `{code, kind, period, limit, usage,
  attempted_amount, config_version}`, under the existing action names
  (`casino_bet.denied_by_rg_policy`, etc.).
- Config writes audit before and after values plus `attestation_ref`. Platform floor
  changes become **tenant-visible** through the ADR 0104 platform-actions feed. Player
  history is tenant-scoped and visible to that tenant's compliance staff, plus the
  player's own history.
- **Alerts** (`internal/alerting`, ADR 0102). Delivery is still **NOT IMPLEMENTED**
  (ALERT-DELIVERY-1).
  - `rg_evaluation_unavailable`
  - `rg_config_missing` (deduplicated per tenant/brand/jurisdiction)
  - `rg_deposit_limit_exceeded_post_facto`
  - `rg_staff_increase_on_player_request` (info)

### 5. API and screens (OpenAPI in `docs/api/openapi/platform-api.yaml`)

**Player** (`RequirePlayerPrincipal`; tenant, account and person from the session
only; `Idempotency-Key` required on writes):
- `GET /v1/me/rg/limits` returns effective limits, pending changes with
  `effective_at`, remaining usage per period, and the allowed periods/ceilings from
  config.
- `POST /v1/me/rg/limits` with `{kind, asset_code?, period?, value|null}` sets,
  decreases, increases or removes a limit. The server classifies the request and
  returns 202 with `effective_at` for a pending increase.
- `POST /v1/me/rg/limits/changes/{id}/confirm` and `…/cancel`.
- `POST /v1/me/rg/time-out` with `{duration}`.
- `POST /v1/me/rg/self-exclusion` and `GET /v1/me/rg/status` exist already
  (`internal/httpserver/rg_routes.go:21-22`). The status response gains `timed_out`.
- `GET|PUT /v1/me/rg/reality-check` sets the interval preference. The client shows the
  notification. In-game reality checks via provider launch params are
  **PROVIDER DEPENDENT**.

**Staff** (`RequireTenantScope`):
- `GET /v1/admin/players/{id}/rg/limits` (+ `/history`), using the new
  `PermRGLimitRead`.
- `POST /v1/admin/players/{id}/rg/limits` uses `PermRGLimitWriteOnPlayerRequest`,
  which requires step-up (ADR 0017), `reason_code` and `evidence_ref`. It applies the
  **same** classification and cool-down as player requests. Staff cannot shorten a
  cool-down, backdate, or create an immediate increase, so there is no silent raise.
  `GET /v1/admin/rg/restrictions` gains a `time_out` filter.
- `GET|POST /v1/admin/rg/limit-configs` takes tenant/brand overrides, tighten-only,
  with `PermRGConfigWrite`.
- `POST /v1/admin/platform/rg/limit-config-change-requests` (+ approve/reject) handles
  jurisdiction floors. It is platform principal only, with **four-eyes** and signed
  actor proof (ADR 0099/0110 pattern) and a mandatory `attestation_ref`.

**Screens**
- **B2C** (`b2c/src`):
  - RG hub (limits list with remaining usage);
  - set/change limit form showing a "takes effect at" preview;
  - pending-increase confirm/cancel;
  - time-out picker;
  - self-exclusion (exists);
  - reality-check preference and timed notification;
  - session-limit expiry screen (withdraw link stays available).
- **Back Office** (`backoffice/src/features/rg`):
  - player RG tab (limits, history, pending changes, restrictions);
  - "apply on player request" form;
  - tenant RG config (overrides with floor shown);
  - platform floor change-request queue with approve/reject;
  - the RG queue gains time-out.
- **Partner console:** read the tenant config and floors.

### 6. Unconfigured configuration ⇒ behaviour

- **No floor row in force** for the brand's licence jurisdiction and kind means that
  **deposits, casino launches and wagers, and sportsbook wagers are refused** with
  `rg_config_missing`. These are unaffected:
  - withdrawals;
  - wins, settlements and stake returns;
  - login;
  - balance and history reads;
  - every protective action.
- **Partial configuration cannot exist**: every field is explicit, and `none` is a
  recorded, attested choice by a named actor.
- **Unconfigured cool-down** cannot happen, since it is a required field. If it ever
  did, increases would be refused.
- A **brand launch-readiness item** "RG config present for every enabled kind" is
  added to the ADR 0112 / gap-audit §0.2 checklist.
- Dev and test fixtures seed configs labelled `SYNTHETIC-NOT-LEGAL` with
  `attestation_ref='synthetic'`. A startup check refuses such rows outside dev/test.
- Whether missing config fails closed (as proposed) or runs permissive-with-alert is a
  **legal/compliance decision** (OQ-1). Consequence of the recommendation: once S4
  merges, the existing Anjouan B2C brand cannot take deposits or wagers until
  compliance supplies or attests a floor (OQ-2).

### 7. Test plan (owner: `qa`; integration tests against real Postgres, `-race`)

- **Invariants:**
  - an increase is never effective before `requested_at + cooldown`;
  - a decrease is always effective immediately;
  - the effective limit is ≤ the composed ceiling;
  - `usage + amount > limit` ⇒ zero ledger rows and zero intent progression;
  - gating never reaches L3 before L0.4;
  - `player_rg_limit_changes` and `rg_limit_configs` reject UPDATE, DELETE and
    TRUNCATE.
- **Adversarial:**
  - client-supplied tenant, person or `effective_at`, or direction, is ignored or
    rejected;
  - backdated floor → rejected;
  - tenant loosening (each field) → Go and DB both reject it, with an audited denial;
  - staff without step-up or `evidence_ref` → 403/400;
  - staff increase is still delayed;
  - tenant B staff cannot read tenant A person's limits/history (RLS, including the
    INNER-JOIN lesson of `rg.go:455`);
  - a player cannot read another account's rows;
  - `SYNTHETIC-NOT-LEGAL` rows are refused in a non-dev env;
  - for each comparator, mutation-kill evidence (`<` vs `<=`, realization rule,
    min-compose).
- **Concurrency:**
  - N parallel casino bets across two brands of one person against a `tenant_person`
    wager limit: at most the allowed total posts;
  - limit decrease vs. bet;
  - time-out vs. launch;
  - parallel deposit T1s against a deposit limit (in-flight counted);
  - increase confirm vs. new decrease;
  - config tighten vs. bet;
  - no deadlock under a mixed bet/rollback/bonus-convert/limit-write load (extend the
    ADR 0082 lock-order suite).
- **Lifecycle:**
  - rolling-window boundary;
  - calendar boundary in a non-UTC timezone, including a DST transition;
  - week-start;
  - multi-asset isolation;
  - `NUMERIC(38,0)` exponent-18 asset;
  - rollback/void restores wager and loss capacity;
  - a win after the loss limit is reached still settles;
  - Q-GP-5 stake return during time-out;
  - withdrawal while the deposit limit is exhausted, while timed out, and while
    `rg_config_missing`;
  - pending increase superseded;
  - reconfirmation required;
  - session deadline mid-round (next bet declined, win posts);
  - refresh refused past the deadline.
- **Fail-closed:** inject errors in the config, history, usage and restriction reads
  at each hook point. Expect no posting, no session and an alert row. Also test an
  undeclared ledger leg.
- **Cross-brand:**
  - self-exclusion or a `platform_person` time-out on brand A blocks brands B and C
    and another tenant;
  - a `tenant_person` limit aggregates across brands but not tenants;
  - MOCK resolver Match/Uncertain registration inherits the person's limits and
    restrictions.

### 8. Ordered slices (each: security + code-reviewer + qa review; the stage gate applies)

| # | Slice | Owner | Required reviewers |
|---|---|---|---|
| S1 | Migrations: `rg_limit_configs`, `player_rg_limit_changes` (+ immutability, RLS, tighten-only/no-backdate triggers), `time_out` CHECK, `casino_launch_sessions.auth_session_root_started_at`, deposit-intent index. Pure Go resolution (realization rule, strictest-compose) + unit/integration tests. No enforcement | identity-compliance | architect, security, qa |
| S2 | Write paths + APIs + audit: player/staff limit changes, time-out, config admin (tenant tighten-only; platform four-eyes + actor proof), OpenAPI, permissions | identity-compliance | security, qa, code-reviewer |
| S3 | Usage measurement: exported read-only `risk` measurement (wager/loss), payments deposit-usage helper, EXPLAIN + p95 evidence | identity-compliance with risk/payments owners | **ledger-finance**, qa |
| S4 | `rg.EvaluateAction` + wiring at the §3.2 hooks, auth refresh session check, launch-token cap, alerts, fail-closed, ADR 0082 amendment note | identity-compliance with casino/sportsbook/payments owners | **ledger-finance** (money-path hooks), security, architect, qa |
| S5 | B2C + Back Office + partner console screens (§5) | frontend specialists | security (authz/UI-not-trusted), qa |
| S6 | Adversarial/concurrency/cross-brand suite (§7) + launch-readiness item + runbook "RG config onboarding" | qa | identity-compliance, code-reviewer |

S4 must not merge before S1–S3, and its rollout requires the OQ-1 answer.
Labels after S6: the mechanism is `IMPLEMENTED`; per-jurisdiction content is
`BLOCKED` on legal; cross-brand person matching is `PROVIDER DEPENDENT`; in-game
reality checks are `PROVIDER DEPENDENT`.

### 9. Owner / legal questions (minimum set)

- **OQ-1 (owner + compliance):** When no RG floor configuration is in force for a
  brand's jurisdiction, do we **refuse** deposits and new wagers (fail closed), or
  allow them and raise an alert?
  **Recommendation:** fail closed. Withdrawals, settlements and protective actions
  remain allowed in both options.
- **OQ-2 (legal/compliance, content):** For Anjouan (the only fixed jurisdiction),
  supply, or explicitly attest as `none`, each §2.1 field. Fields per kind:
  - enabled/mandatory;
  - ceiling;
  - allowed periods and timezone;
  - increase cool-down and reconfirmation;
  - scope;
  - bonus-funded counting;
  - time-out durations and scope;
  - reality-check intervals/mandatory.

  Also name the compliance signatory for `attestation_ref`.
  **Recommendation:** route this through the existing licence counsel. Engineering
  will not propose values.
- **OQ-3 (owner):** Confirm that monetary and session limits are scoped at most to
  "one Person within one operator (tenant)", and that only self-exclusion and time-out
  apply across operators.
  **Recommendation:** confirm. Cross-operator amount aggregation would require
  cross-tenant ledger reads and cross-operator data sharing that the hybrid licensing
  model (ADR 0006) does not authorize.

## Consequences

- The Blueprint RG controls get a single enforcement boundary with no new hot-path
  lock and ledger-derived (non-drifting) usage. There is a measurable per-bet read
  cost, to be evidenced in S3.
- With OQ-1 as recommended, every brand is non-transactable until compliance supplies
  configuration. This is deliberate, because the alternative would be an implied
  legal default.
- `player_restrictions` grows a second type. ADR 0026 §15 anticipated this.
- Deferred: cumulative play-time limits, operator-imposed affordability limits,
  cross-tenant amount limits, a per-player-jurisdiction overlay and WD-RG-1.
