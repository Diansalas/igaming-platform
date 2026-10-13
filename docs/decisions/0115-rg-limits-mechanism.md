# ADR 0115 — Responsible Gaming limits, time-out and jurisdiction-aware RG configuration

## Status

**ACCEPTED, pending implementation** (architect, 2026-10-12). Revision 2 adds the
ledger-finance and security design reviews (both APPROVE WITH CONDITIONS, ledger-finance
with 2 blockers). Every condition is applied below as ADR text. The review
disposition table is at the end. Design only: **nothing here is implemented.**
The labels are `NOT IMPLEMENTED` until slices S1–S6 (§8) merge.

**Pointer (2026-10-13):** ADR 0119 (`0119-staff-mfa-stepup-and-lifecycle.md`) replaces ADR 0017 as the step-up
mechanism. Every "until ADR 0017 ships" below now reads: staff increases and removals stay 403 until **ADR 0119 S4b**
(step-up enforcement) **and** this ADR's own classifier slice have both merged. Under ADR 0119 §8.2, a staff increase or
removal is `critical` step-up and a decrease is `none`. The `rg_limit_config:request|approve|apply` operations are in the
ADR 0119 §9.1 signer backstop registry (`critical`).

Owner directive 2026-10-11: finish provider-independent RG. No legal value is
chosen here. Every numeric limit, cool-down, period, duration and cadence is
**configuration per jurisdiction**, and the configuration has **no default that
implies legal compliance**. Compliance/legal supplies the content. The OQ-1..OQ-3
answers in §9 are **orchestrator defaults, reversible by owner/legal**. They are not
owner or legal approvals. Migration numbers are allocated by the orchestrator only.

## Context

- **Blueprint requirement.** As extracted in `docs/architecture/01-requirements-inventory.md` §6
  and `docs/architecture/11-kyc-aml-rg-architecture.md` "Responsible gaming", the
  platform needs deposit, loss, wager and session limits. A decrease takes effect
  immediately; an increase only after a cooling-off period. It also needs reality
  checks, time-outs, and self-exclusion at brand and platform level. A permanent
  self-exclusion survives account closure and re-registration (person cluster).
- **Implemented today** (`internal/rg/rg.go`, ADR 0026, migrations 0037/0038):
  - Only one restriction type exists, `self_exclusion` (CHECK in
    `migrations/0037_create_player_restrictions.up.sql:49`). Rows are append-only,
    enforced by immutability triggers.
  - Player self-service exclusions apply platform-wide. Staff exclusions are tenant- or
    brand-scoped.
  - `EvaluateEligibility` (`rg.go:584`) takes the L0.4 person advisory lock
    (`rg.go:138`). It then checks account status, self-exclusion (using
    `clock_timestamp()`) and wallet status.
  - Supporting pieces: the open-bet policy (`self_exclusion_policy.go`, ADR 0034 §14,
    the effective-dated, tighten-only, no-backdating config pattern), the enumeration
    sweep (`enumeration_sweep.go`) and Person resolution (`internal/identityresolution`,
    ADR 0027, **MOCK** resolver only).
- **Current `EvaluateEligibility` callers:**

  | Call site | Location | Ordering notes |
  |---|---|---|
  | Casino launch | `internal/casino/orchestrator.go:399` | |
  | Casino bet | `orchestrator.go:1647` | after L0.1 `:1408` and L0.2 `:1630`; before risk `:1688` and L3 `:1805` |
  | Sportsbook place | `internal/sportsbook/orchestrator.go:279` | before L3 `:483` |
  | Deposit T1 | `internal/payments/deposit_v2.go:207` | |
  | Deposit T2 | `internal/payments/drive.go:88` | |
  | Bonus eligibility | `internal/bonus/eligibility.go:95` | |

  Rollback, win, void and settlement paths deliberately do not call it
  (`casino/orchestrator.go:1954,2096`).
- **Gap audit** (`docs/governance/platform-completion-gap-audit-2026-10-10.md` §7):
  there are no deposit, loss, wager or session limits, no time-out, and no cool-down on
  limit increases.
  - Risk rules (ADR 0031) are **operator** limits and are not reused as RG limits.
  - Risk's *specs* also cannot be reused for RG usage. ledger-finance BLOCKER-2 explains
    why (§3.4).
- **Constraints:**
  - ADR 0082 lock order.
  - Q-GP-5: terminal stake returns are allowed (task-registry
    `DECISIONS-PRH2-FINAL-GAMEPLAY-SECURITY-2026-10-06`).
  - WD-RG-1 is open (`docs/HANDOVER.md:481`).
  - ADR 0017 step-up is NOT IMPLEMENTED. The withdrawal path fails closed on it
    (`internal/httpserver/withdrawal_handlers.go:788`).
  - `tenant_jurisdiction_configs` has no reader and holds opaque ruleset ids, so it is
    not reused.

## Decision

### 1. Controls, scopes, periods, usage

| Control | Kind key | Unit | Scope options (config) | Period |
|---|---|---|---|---|
| Deposit limit | `deposit` | asset minor units, `NUMERIC(38,0)` | `account` \| `tenant_person` | yes |
| Loss limit | `loss` | asset minor units | `account` \| `tenant_person` | yes |
| Wager limit | `wager` | asset minor units | `account` \| `tenant_person` | yes |
| Session duration limit | `session_duration` | seconds | `account` \| `tenant_person` | no (per login chain, §1.5) |
| Reality-check cadence | `reality_check_interval` | seconds | `account` | no. **Notification only**; never blocks |
| Time-out / cooling-off break | restriction `time_out` | duration | `platform_person` \| `tenant_person` \| `account` | n/a |
| Self-exclusion (exists) | restriction `self_exclusion` | duration or indefinite | `platform_person` (player); tenant/brand (staff) | n/a |

**1.1 Scope.**
- Self-exclusion stays person-level and platform-wide for self-service (ADR 0026 §3).
- An unconfigured time-out scope defaults to **`platform_person`**. That is the
  protective choice; it is not a legal value.
- Amount and session limits reach at most **`tenant_person`**: one Person's accounts
  across the brands of one tenant. They never cross tenants (OQ-3).
- `tenant_person` aggregation depends on Person linking, which is **PROVIDER
  DEPENDENT** (MOCK, ADR 0027). §2.5 restricts what a linked account may do.

**1.2 Periods.**
- Allowed values: `rolling_24h`, `rolling_7d`, `rolling_30d`, `calendar_day`,
  `calendar_week`, `calendar_month`.
- Window starts are computed **in SQL from `clock_timestamp()`**:
  - Rolling windows subtract `interval '24 hours' / '168 hours' / '720 hours'`. They
    never use `'1 day'`, which is DST-sensitive.
  - Calendar windows are
    `date_trunc(unit, clock_timestamp() AT TIME ZONE calendar_tz) AT TIME ZONE calendar_tz`.
    Week start comes from config.
  - Go local time is never used.
- A player may hold several limits of one kind, and all of them apply.

**1.3 Assets.**
- Amount limits are per asset code (Asset-registry exponent, ADR 0007/0021), with no
  FX conversion.
- If a kind is `mandatory`, every asset the player transacts in needs a limit.

**1.4 Usage definitions.**
- **Source.** Usage is always derived from the ledger or from `deposit_intents` at
  read time. There are **no counters, so usage cannot drift**. A future rollup would
  be a ledger-derived projection covered by the hourly drift job (CLAUDE.md), never
  authoritative.
- **wager.** The sum of stake legs (`casino_bet`, `sportsbook_bet`), minus reversals
  **whose `reverses_transaction_id` is a stake** (a rollback of a `casino_win` never
  counts). Bonus-funded legs count only if `count_bonus_funded=true`.
- **loss.** The net debit to `player_cash` (plus `player_bonus` when
  `count_bonus_funded=true`) over the transaction types `casino_bet`, `casino_win`,
  `casino_rollback`, `sportsbook_bet`, `sportsbook_settlement`, `sportsbook_void` and
  `sportsbook_rollback`. `player_locked_cash` and `player_locked_bonus` are **declared
  ignored**.
  - When `count_bonus_funded=false`, wins and returns on bonus-funded legs are
    excluded too.
  - Bonus conversion (bonus to cash) **never reduces** loss usage.
  - `loss_computation` supports only `net_of_wins`. A config row naming anything else
    is rejected at write time.
  - Whether net loss may go negative (a net win giving extra headroom) is the attested
    field `loss_may_go_negative`. When the config row is present but this field is
    `none`, the engineering floor of 0 applies.
- **Reversal window rule.** A reversal counts toward wager or loss **only if its
  original transaction is inside the same window**.
- **deposit.** Intents with status in {`pending`, `ambiguous`} count at the amount
  requested. `succeeded` intents count at the amount credited by their ledger deposit
  posting. Inbound deposits with no intent are counted from ledger deposit postings.
  - `deposit_intents.status` has no `expired` value (migration 0025 CHECK:
    `pending|succeeded|declined|ambiguous|failed`). A pending intent older than a
    configured TTL is moved to `failed` by the **existing payments sweeper**, which
    releases its usage.
  - Reversals and withdrawals do not restore capacity.
  - For `tenant_person`, deposit usage is **joined by `person_id`** through the
    tenant's `player_accounts`, never by `player_account_id` alone.

**1.5 Session.**
- `session_duration` is measured from the `created_at` of the root of the
  refresh-token rotation chain (`migrations/0012_create_sessions.up.sql`,
  `replaced_by_session_id`).
- Re-login and parallel chains can bypass it. Two **required, attestable** config
  fields cover this, each taking a value or `none`:
  - `post_session_break`: minimum interval after a chain hits its limit before a new
    chain may wager or deposit.
  - `concurrent_chain_aggregation`: whether all of the Person's open chains in the
    scope count from the earliest root.

**1.6 Deferred (recorded, not built).** Cumulative play time per period, operator-imposed
affordability limits, cross-tenant amount limits, a per-player-jurisdiction overlay,
and WD-RG-1.

### 2. Data model

**2.1 `rg_limit_configs` (versioned; pattern of migrations 0043/0049)**

- **Key.** `(jurisdiction_code, tenant_id NULL, brand_id NULL, control_kind, effective_from)`.
  - `tenant_id NULL` marks the **jurisdiction floor**.
  - Tenant and brand rows are **operator bounds**.
- **Fields.** Every field is explicit. Each holds either a value or the literal `none`
  (an attested "no bound"). A missing field is rejected by a DB `CHECK`, so partial
  configuration cannot exist. The fields:
  - `enabled`, `mandatory`
  - `ceiling` (value | `none`): the hard cap and the maximum a player may set
  - `allowed_periods[]`, `calendar_tz`, `week_start`
  - `increase_cooldown` (interval; no `none`; zero rejected)
  - `increase_requires_reconfirmation`
  - `scope`
  - `count_bonus_funded`
  - `loss_computation` (`net_of_wins`), `loss_may_go_negative`
  - `post_session_break`, `concurrent_chain_aggregation` (for `session_duration`)
  - `pending_intent_ttl` (for `deposit`)
  - Time-out rows: `allowed_durations[]` | `any`, plus `scope`
  - Reality-check rows: `allowed_intervals[]`, `mandatory`
  - On every row: `reason_code`, `attestation_ref`, `created_by_*`, `created_at`
- **Append-only.** An immutability trigger enforces this. The row in force is the
  latest `effective_from <= asOf` for the exact scope.
- **No backdating, for floor or operator rows.** This reuses the
  `ErrJurisdictionFloorBackdated` rule and its 5 s DB-trigger tolerance (security
  condition 9).
- **Operator rows are tighten-only for restrictive fields and widen-only for
  protective options** (security condition 1). Both the Go layer (with an audited
  denial) and a DB trigger check this.
  - **Restrictive fields may only tighten:**
    - lower `ceiling`
    - longer `increase_cooldown`
    - `mandatory` false→true
    - reconfirmation false→true
    - `count_bonus_funded` false→true
    - `loss_may_go_negative` true→false
    - scope `account`→`tenant_person`
    - longer `post_session_break`
    - `concurrent_chain_aggregation` false→true
  - **Protective options may only widen:**
    - `enabled` false→true (an operator may let players set a limit the floor does
      not offer, but may never take one away)
    - `allowed_periods`, `allowed_durations` and `allowed_intervals` may only be
      **supersets** of the floor
- **Read-time composition** works even if a trigger is bypassed:
  - strictest value for restrictive fields
  - **UNION** for option sets
  - **OR** for `enabled` and `mandatory`
- **Floor four-eyes is enforced in the DB** (security condition 9, ADR 0113 pattern).
  - A floor row can only be inserted with `change_request_id` referencing an
    `approved` row in `rg_limit_config_change_requests`.
  - The trigger checks that the approver's `person_id` differs from the requester's
    `person_id`.
  - The signed actor proof (ADR 0110) is bound to the request id **and** a hash of the
    payload; the trigger recomputes the hash from the inserted row.
- **Synthetic-config guard** (security condition 8). Rows with
  `attestation_ref='synthetic'` are labelled `SYNTHETIC-NOT-LEGAL`.
  - Resolution treats such rows as **absent**, which yields `rg_config_missing` plus an
    alert, unless `cfg.GuardEnvironment()` (`internal/config/config.go:924`) is
    `development` or `test`. A missing `APP_ENV` counts as production, and staging
    counts as non-dev.
  - A DB trigger refuses synthetic rows unless the GUC `app.environment` is
    `development` or `test`. This GUC is **new**: S1 sets it per connection from the
    guarded config, and when it is absent the trigger treats the environment as
    production.
  - This replaces the earlier "startup refusal", which was an availability hazard.
- **RLS.**
  - Floors are readable by all; tenant and brand rows only by
    `tenant_id = app.tenant_id`.
  - Floor writes need a platform principal plus an approved change request. Overrides
    are written by the same tenant (as in migration 0090).
- **Jurisdiction key.** Derived server-side from the tenant licence, as in
  `jurisdiction.EvaluationPolicyLookup` (`internal/jurisdiction/evaluation_policy.go`).
  It is never taken from a request body.

**2.2 `player_rg_limit_changes` (append-only; the authoritative player-set state)**

- **Columns:**
  - `seq BIGSERIAL` (ordering tiebreaker) and `id`
  - `tenant_id NOT NULL`, `person_id`, `player_account_id` (anchor), `brand_id`
  - `scope`, `control_kind`, `asset_code NULL`, `period_kind NULL`
  - `value NUMERIC(38,0) NULL` (NULL means removal)
  - `change_kind` (`initial|decrease|increase|removal|cancel_pending|confirm`)
  - `requested_at`, `effective_at`
  - `supersedes_change_id NULL`
  - `source` (`player_self_service|staff_on_player_request`)
  - `reason_code`, `evidence_ref`
  - `created_by_actor_type/id`
  - `config_version_id`
  - `idempotency_key`, `request_hash`, with
    `UNIQUE (tenant_id, player_account_id, idempotency_key)`
- **The server sets the time and the classification** (security condition 3).
  - A `BEFORE INSERT` trigger sets `requested_at` from `clock_timestamp()`.
  - It sets `effective_at` from the config **in force at that instant**: decreases,
    initial values and `cancel_pending` apply now; increases and removals apply at
    now + `increase_cooldown`.
  - **One SQL function** implements both the `change_kind` classification and the
    realization rule. The trigger calls it and rejects any asserted `change_kind` or
    `effective_at` that differs. Go calls the same function and never re-implements
    it.
  - Ordering is `(requested_at, seq)`.
- **Idempotency.** Reusing an `Idempotency-Key` with a different body (different
  `request_hash`) returns **409**.
- **Realization rule.** Requests for one key are ordered by `(requested_at, seq)`.
  - A row is *realized* iff `effective_at <= min(t, requested_at of the next request)`.
  - The effective player value is the latest realized row.
  - An `increase` row that needs reconfirmation is never realized itself. A later
    `confirm` row, allowed only once `t >= effective_at` and with no newer request,
    carries the value.
  - **A staff-originated increase or removal always needs a `confirm` from the
    player's own authenticated session**, whatever `increase_requires_reconfirmation`
    says (security condition 4).
- **Effective limit across scopes** (security condition 5). The effective limit is the
  **MIN** of:
  - every realized account-level row for the anchor account
  - every realized `tenant_person` row for that Person within the tenant
  - the composed `ceiling`

  It is never +∞ where any limit existed. Changing scope from account to
  `tenant_person` therefore cannot drop a limit.
- **Ceiling changes.**
  - Lowering a ceiling clamps the effective limit immediately.
  - **Raising a ceiling** that lifts a clamped player's effective limit takes effect
    for that player only after the player's `increase_cooldown`, measured from the
    ceiling row's `effective_from`.
- **Mandatory kinds.** If a kind is `mandatory`, the player has no value, and the
  ceiling is `none`, the action is refused with `rg_limit_required`.
- **Pending cap.** Each player may have at most a configured number of open pending
  changes (security condition 6). The cap is an engineering anti-abuse bound, not a
  legal value.
- **RLS.**
  - Tenant isolation applies (`WithTenant`).
  - Players can self-read and self-insert via `app.player_account_id`. `person_id` and
    `tenant_id` are re-derived server-side.
  - There is no platform-wide read. **Limit history never leaves its tenant.**
- **Indexes:**
  - `(tenant_id, person_id, control_kind, asset_code, period_kind, requested_at DESC, seq DESC)`
  - `(tenant_id, player_account_id, control_kind, …)`

**2.3 `player_restrictions`.**
- The CHECK is extended to `('self_exclusion','time_out')`. Locks, RLS and
  immutability are unchanged.
- `EvaluateEligibility` returns `self_excluded` or `timed_out`.
- A time-out cannot be ended early, and it **never changes wallet status**, so
  withdrawals keep working.
- Player self-insert of a `time_out` **explicitly extends the `player_self_insert`
  RLS policy** (security condition 10). For `tenant_person` scope, `tenant_id` is
  pinned server-side from `app.tenant_id`, never from the body.

**2.4 `casino_launch_sessions`.** Add `auth_session_root_started_at`, recorded at
launch. Bets recompute the deadline from the *current* limit.

**2.5 Linked accounts** (security condition 7).
- Only an account linked to the Person by a resolver **Match** may request an increase
  or removal of a `tenant_person` limit. Every linked account is notified.
- An **Uncertain** registration follows ADR 0027 §5/§6: it gets a new Person in
  `identity_review_required`. It gets **no** person-level privileges and no
  inheritance until review links it. This corrects revision 1's "Match/Uncertain
  inherits" wording, which conflicted with ADR 0027.
- After a confirmed link, restrictions and limits apply through the Person.

**2.6 Indexes** (ledger-finance BLOCKER-1).
- The revision 1 claim was false. `risk/cumulative.go:317` filters on
  `(tenant_id, player_account_id, asset_code, created_at)`, and no index serves that
  shape. The only one is the partial `idx_ledger_entries_player_account`, so the query
  scans the player's whole history on every bet.
- S1 adds `ledger_entries (tenant_id, player_account_id, asset_code, created_at)`,
  built without blocking writes (the `CONCURRENTLY` practice from
  DEVOPS-0107-INDEX-WINDOW-1, in its own non-transactional migration step).
- S1 also adds `deposit_intents (tenant_id, player_account_id, asset_code, created_at)`.
- **Amendment (S1 implementation, ledger-finance review LF 3).** This repository's migration
  runner applies every file in one transaction, so there is no "own non-transactional migration
  step". Instead the operator pre-builds both indexes `CONCURRENTLY` out of band before
  migration 0135 (runbook `expand-contract-migrations.md` section 5.1). 0135's first step accepts
  a pre-built index only if VALID and identically defined and then executes nothing against the
  table. A missing index is built in the migration only on a small table; above the threshold
  (`reltuples > 100000` or more than 64 MB) the migration refuses with `RL098`. Two §2.2 shapes
  also changed in S1:
  - the singular `config_version_id` is `config_version_ids uuid[]`, because a composed
    configuration spans several rows;
  - a `requires_confirmation` column records that a row awaits the player's `confirm`.

### 3. Enforcement

**3.1 One boundary: `rg.EvaluateAction(ctx, tx, ActionParams{...})`.**

- **Parameters:** `TenantID`, `BrandID`, `PlayerAccountID`, `WalletID`, `Action`,
  `AssetCode`, `Amount`, `CandidateRef`, `AuthSessionRootStartedAt`.
- **`Action` values:** `deposit|casino_launch|casino_wager|sportsbook_wager|session_refresh`.
- **Isolation guard.** `EvaluateAction` **refuses a `REPEATABLE READ` or
  `SERIALIZABLE` transaction** with an error, so it fails closed. The usage `SELECT`
  runs as a new statement after the L0.4 lock is acquired, under `READ COMMITTED`, so
  it sees every commit that preceded the lock. A concurrency test pins this.
- **Evaluation order for all actions except `session_refresh`:**
  1. `EvaluateEligibility` (L0.4, status, self-exclusion, time-out).
  2. Config resolution. No floor in force gives `rg_config_missing`.
  3. Session deadline, including `post_session_break` and
     `concurrent_chain_aggregation`.
  4. Amount limits: allowed iff `usage + amount <= limit`.
- **`session_refresh`** (security condition 2) evaluates **only the session
  deadline**.
  - It never returns `self_excluded`, `timed_out`, `rg_config_missing` or
    `rg_limit_required`.
  - It **never refuses login**: a new chain always starts, so excluded or timed-out
    players can still withdraw.
  - Past the deadline, the *existing* chain's refresh is refused and the player must
    log in again. Wagering or depositing in the new chain is still subject to
    `post_session_break`.
- **Protective writes never call `EvaluateAction`.** These are decrease,
  `cancel_pending`, time-out and self-exclusion. They succeed whatever the
  `enabled`/config/restriction state, and a time-out with unconfigured durations
  accepts any positive duration.
- **New decision codes:** `timed_out`, `rg_config_missing`, `rg_limit_required`,
  `deposit_limit_reached`, `loss_limit_reached`, `wager_limit_reached`,
  `session_limit_reached`.
- **Single boundary preserved.** ADR 0026 §5's single-boundary rule still holds. Bonus
  eligibility keeps `EvaluateEligibility`.

**3.2 Hook points.** No call is moved.

| Path | File:line | Checks | Note |
|---|---|---|---|
| Deposit T1 | `payments/deposit_v2.go:207` | deposit, mandatory, config | `CandidateRef` = own intent: excluded from usage, added once as `Amount` |
| Deposit T2 | `payments/drive.go:88` | same, re-checked | An RG denial sets the intent to `declined` **in the same transaction** |
| Deposit receipt (`receipt.go`) | — | none | A late provider success on an RG-declined or failed intent is **credited** through `receipt.go`. It is never auto-refunded. It raises `rg_deposit_limit_exceeded_post_facto`, and the funds stay withdrawable |
| Casino launch | `casino/orchestrator.go:399` | config, session, mandatory | Launch-token `expires_at` capped at the session deadline (ADR 0103) |
| Casino bet | `casino/orchestrator.go:1647` | wager, loss, session | Runs after the idempotency short-circuit. Bonus-funded legs per §1.4 |
| Sportsbook place | `sportsbook/orchestrator.go:279` | wager, loss (stake = potential loss), session | Bonus-funded sportsbook stakes remain blocked (`:120`) |
| Auth refresh | `internal/auth` rotate | `session_refresh` only (§3.1) | Never blocks login |
| Wins, rollbacks, voids, settlements, Q-GP-5 stake returns | `casino/orchestrator.go:1954,2096`, `sportsbook/settlement.go` | none | Never gated |
| Withdrawals | `internal/withdrawal`, `payments/payout.go` | none | Never blocked by limits, time-out or `rg_config_missing`. WD-RG-1 separate |
| Bonus claim | `bonus/eligibility.go:95` | eligibility | A grant or conversion is not a deposit |

**3.3 Locks and ordering (ADR 0082).** No new hot-path lock class.
- All checks run inside the existing L0.4 lock at its current position: after the
  status keys, L0.1 and L0.2; before L0.5 and L1–L3 (R8).
- **Limit-change and time-out writers take only L0.4.** They never then take a tenant
  or brand status gate key, or any L1–L4 lock. These writers are added to the ADR 0082
  **static lock-order suite**.
- Config writers take a writer-only scope advisory that is never co-held with L0.x or
  L1+. This is recorded as an ADR 0082 amendment note.
- **Timeouts** (security condition 6):
  - `lockPerson` runs `SET LOCAL lock_timeout` first.
  - Usage scans run under `SET LOCAL statement_timeout`.
  - Either timeout fails closed and raises `rg_evaluation_unavailable`.
  - The values are engineering operational settings, set by devops and measured in S3.

**3.4 Usage measurement** (ledger-finance BLOCKER-2).
- Risk's `casino_bet` spec cannot be reused, for three reasons:
  - It nets every `casino_rollback`, so rolling back a `casino_win` would count as
    wager.
  - Bonus-funded casino bets touch `player_bonus`/`player_locked_bonus`
    (`casino/orchestrator.go:2281`), which `casino_bet` does not declare, so the
    measurement would hit `ErrUnrecognizedCumulativeLeg`.
  - No spec covers wins.
- **ledger-finance supplies new RG-only specs** (wager and loss as defined in §1.4),
  using risk's declared-leg / undeclared-leg-fails-closed mechanism. **Risk's own specs
  are unchanged.**
- **One scan per action.** It uses `player_account_id = ANY(<Person's accounts in this
  tenant>)` (or the anchor account alone for account scope) and computes every
  window's total with `SUM(...) FILTER (WHERE created_at >= window_start_i)`.
- Deposit usage uses one scan of `deposit_intents` plus ledger deposit postings for
  intent-less deposits.
- **Before S4 merges, S3 must provide:**
  - `EXPLAIN (ANALYZE, BUFFERS)` showing the §2.6 indexes in use
  - the p95 latency delta on casino bet, sportsbook place and deposit T1
  - **L0.4 lock hold time**

  No SLA is claimed. Redis or any cache is never consulted.

**3.5 Fail-closed.**
- Any read error aborts the transaction: nothing posts, no intent proceeds, no session
  is minted.
- An undeclared ledger leg, an isolation-guard failure, or a lock or statement timeout
  all count as errors.
- The caller logs out-of-transaction and emits `rg_evaluation_unavailable` (ADR 0102),
  as `launchFailed` does.
- **Abuse bounds.** Per-player rate limits apply on every `/v1/me/rg/*` write and on
  deposit T1 (security condition 6).

### 4. Audit, alerts, disclosure

- **Change audit.** Every limit request is audited in the transaction, whether
  accepted or refused (`rg.limit.change_requested|…_denied|confirmed|cancelled`). Each
  entry records:
  - actor, tenant, person, account, kind, asset, period
  - effective value before and after, pending value, `effective_at`
  - config version, reason, IP, UA, request id
- Time-outs are audited as `rg.time_out.created`.
- **Denial audit** captures enough to replay the decision (ledger-finance):
  - `{code, kind, period, limit, usage, attempted_amount, config_version}`
  - `window_start`
  - the usage SQL inputs: account set, asset, transaction-type set, `clock_timestamp`
- **Staff requests** appear in the player's own RG history and trigger a player
  notification (security condition 4).
- **Config audit.** Config writes record before/after and `attestation_ref`. Floor
  changes are tenant-visible through the ADR 0104 feed.
- **Cross-tenant disclosure** (security condition 10). Disclosure of `time_out` or
  `self_exclusion` across tenants is limited to **type, platform scope, start and
  end**. Every new reader (BO tab, exports, ADR 0104 feed) must either use a redacting
  DTO or follow the `ListRestrictionsForTenant` INNER-JOIN pattern (`rg.go:455`).
- **Daily check.** Compares credited amounts on `succeeded` intents against ledger
  deposit postings. A mismatch raises an alert.
- **Alerts** (ADR 0102; delivery is still **NOT IMPLEMENTED**, ALERT-DELIVERY-1):
  - `rg_evaluation_unavailable`
  - `rg_config_missing` (deduplicated)
  - `rg_deposit_limit_exceeded_post_facto`
  - `rg_staff_increase_on_player_request`
  - `rg_synthetic_config_in_non_dev`
  - `rg_deposit_credit_mismatch`

### 5. API and screens (OpenAPI in `docs/api/openapi/platform-api.yaml`)

**Player** (`RequirePlayerPrincipal`; identity from the session only; `Idempotency-Key`
required, with 409 on a body mismatch; rate-limited):
- `GET /v1/me/rg/limits`
- `POST /v1/me/rg/limits` with `{kind, asset_code?, period?, value|null}`. Returns 202
  with `effective_at` when the change is pending.
- `POST /v1/me/rg/limits/changes/{id}/confirm|cancel`
- `POST /v1/me/rg/time-out`
- Existing routes: `POST /v1/me/rg/self-exclusion` and `GET /v1/me/rg/status`
  (`internal/httpserver/rg_routes.go:21-22`). Status gains `timed_out`.
- `GET|PUT /v1/me/rg/reality-check`. In-game delivery is **PROVIDER DEPENDENT**.

**Staff** (`RequireTenantScope`):
- `GET /v1/admin/players/{id}/rg/limits` and `/history` (`PermRGLimitRead`)
- `POST /v1/admin/players/{id}/rg/limits` (`PermRGLimitWriteOnPlayerRequest`). Requires
  `reason_code` and `evidence_ref`.
  - **Decreases** need no step-up.
  - **Increases and removals return 403** until ADR 0017 step-up ships. Even after
    that, they are never realized without the player's own `confirm`.
  - Staff whose `person_id` equals the target's are refused.
- `GET|POST /v1/admin/rg/limit-configs` (operator rows; `PermRGConfigWrite`)
- `POST /v1/admin/platform/rg/limit-config-change-requests` plus approve/reject. Floor
  changes go through this DB-enforced four-eyes flow (§2.1).
- `GET /v1/admin/rg/restrictions` gains a `time_out` filter and redacted
  cross-tenant rows.

**Screens:**
- **B2C** (`b2c/src`):
  - RG hub with remaining usage
  - limit form with a "takes effect at" preview
  - confirm/cancel for pending changes, including staff-originated ones
  - time-out picker
  - reality-check preference and notification
  - session-expiry screen (the withdraw link stays)
- **Back Office** (`backoffice/src/features/rg`):
  - player RG tab (limits, history, pending changes, redacted restrictions)
  - "apply on player request" form
  - tenant config (overrides alongside the floor)
  - floor change-request queue
- **Partner console:** read-only config.

### 6. Unconfigured configuration ⇒ behaviour (orchestrator default OQ-1)

- **No floor in force** for the brand's licence jurisdiction and kind (including a
  synthetic row outside dev/test): **deposits and new wagers are refused, and so are
  casino launches** (`rg_config_missing`).
- **Still allowed:** withdrawals, wins, settlements, stake returns, login, balance and
  history reads, and every protective action.
- **Partial configuration cannot exist** (§2.1).
- **Launch readiness.** A new readiness item, "RG config present for every enabled
  kind", joins the ADR 0112 / gap-audit §0.2 checklist.
- **Test fixture** (ledger-finance condition): `internal/testsupport/rgfix`. It uses
  the integration build tag and is import-pinned like `internal/testsupport/launchfix`.
  - It seeds a permissive `SYNTHETIC-NOT-LEGAL` floor for every jurisdiction that
    `launchfix` provisions: `enabled=false`, `mandatory=false`, `ceiling=none`, a
    non-zero cooldown, and every other field explicit.
  - This keeps the existing money-path integration test files' semantics unchanged.
  - Fail-closed behaviour is proven in dedicated tests that omit the floor.
- **Consequence:** once S4 merges, the Anjouan B2C brand cannot take deposits or new
  wagers until compliance supplies or attests a floor (OQ-2).

### 7. Test plan (owner `qa`; real Postgres, `-race`)

- **Invariants:**
  - an increase is never effective before `requested_at + cooldown`
  - a decrease is effective immediately
  - the effective limit equals the MIN across scopes and ceiling (§2.2)
  - `usage + amount > limit` produces zero ledger rows and no intent progression
  - L0.4 is always taken before L3
  - append-only tables reject UPDATE, DELETE and TRUNCATE
  - a time-out leaves wallet status unchanged (withdraw pre-existing funds while
    `timed_out`)
- **Adversarial:**
  - client-supplied tenant, person, `effective_at`, `change_kind` or timestamps are
    rejected by the trigger
  - an idempotency key reused with a different body returns 409
  - backdated floor or operator rows are rejected
  - an operator loosening any restrictive field, narrowing any option set, or turning
    `enabled` true→false is rejected by both Go and the DB
  - a floor inserted without an approved request, or with the same person approving,
    or with a payload-hash mismatch is rejected
  - synthetic rows are refused by the trigger and resolve as absent when
    `app.environment`/`APP_ENV` is missing or set to staging
  - staff increases return 403 while step-up is not implemented, and are never
    realized without the player's `confirm`
  - staff acting on their own Person are refused
  - an Uncertain-registered account cannot raise a `tenant_person` limit
  - tenant B cannot read tenant A's limit history, and cross-tenant restriction fields
    are redacted
  - rate limits and the pending-change cap are enforced
  - mutation-kill evidence for `<=`, the realization rule, MIN-compose, UNION/OR
    composition and reversal-in-window
- **Concurrency:**
  - N parallel bets across two brands of one Person against a `tenant_person` limit
  - decrease vs. bet
  - time-out vs. launch
  - parallel deposit T1s, with in-flight intents counted
  - confirm vs. a new decrease
  - config tighten vs. bet
  - a `REPEATABLE READ` caller is refused, and the `READ COMMITTED` post-lock read sees
    a concurrent commit
  - lock and statement timeouts fail closed
  - no deadlock under a mixed load, using the ADR 0082 static plus dynamic suites
- **Lifecycle:**
  - rolling and calendar boundaries in a non-UTC timezone across DST, and week start
  - multi-asset; an exponent-18 `NUMERIC(38,0)` asset
  - a rollback of a win does not change wager usage
  - a reversal whose original is outside the window does not count
  - `loss_may_go_negative` both ways
  - bonus-funded legs both ways, and conversion never reduces loss
  - TTL sweep to `failed` releases deposit usage
  - a T2 decline in the same transaction
  - a late success on a declined intent is credited, raises an alert and stays
    withdrawable
  - an intent-less deposit is counted
  - withdrawals while a limit is exhausted, while `timed_out`, while `self_excluded`
    and while `rg_config_missing`
  - `session_refresh` never blocks login
  - `post_session_break` and `concurrent_chain_aggregation`
  - a ceiling raise delayed by cooldown
  - scope change cannot drop a limit
- **Fail-closed:** inject an error into each read at each hook point, an undeclared
  leg, and the isolation guard.
- **Cross-brand:** a platform-wide exclusion or time-out blocks other brands and
  tenants; `tenant_person` aggregates across brands but never across tenants.

### 8. Ordered slices (each also gets code-reviewer + qa; the stage gate applies)

| # | Slice | Owner | Required reviewers |
|---|---|---|---|
| S1 | **Schema + index**: `rg_limit_configs` + change-request table (four-eyes trigger, hash binding, no-backdate, tighten/widen triggers, synthetic trigger, `app.environment` GUC); `player_rg_limit_changes` (BEFORE INSERT trigger + single classification/realization SQL function, `seq`, `request_hash`); `time_out` CHECK + `player_self_insert` extension; `casino_launch_sessions.auth_session_root_started_at`; **non-blocking `ledger_entries` index** and `deposit_intents` index; `internal/testsupport/rgfix` | identity-compliance (index: ledger-finance + devops) | architect, security, **ledger-finance**, qa |
| S2 | **Write APIs**: player/staff limit changes (step-up 403, player confirm, self-target refusal, notifications), time-out, operator config, floor change requests, rate limits, pending cap, 409 idempotency, redacting DTOs, OpenAPI, permissions | identity-compliance | security, qa |
| S3 | **Usage measurement**: RG-only wager/loss specs (ledger-finance-authored), single-scan `FILTER` query, SQL window starts, deposit usage (intents + ledger, person join), TTL sweep to `failed`, daily credit check; EXPLAIN + p95 + L0.4 hold time | ledger-finance with identity-compliance, payments | **ledger-finance**, architect, qa |
| S4 | **Enforcement**: `rg.EvaluateAction` (isolation guard, timeouts, `session_refresh` rules), §3.2 wiring, T2 same-transaction decline, launch-token cap, alerts, ADR 0082 amendment note and static-suite entries | identity-compliance with casino/sportsbook/payments | **ledger-finance**, security, architect, qa |
| S5 | **UI**: B2C, Back Office, partner console (§5) | frontend specialists | security, qa |
| S6 | **QA**: §7 suite, launch-readiness item, "RG config onboarding" runbook | qa | identity-compliance, security, code-reviewer |

**Merge gates:**
- S4 must not merge before S1–S3 **and** before S3's EXPLAIN evidence shows index use.
- S4's rollout makes every brand non-transactable until OQ-2 content exists. The
  orchestrator sequences that rollout with the owner.
- Staff increases stay 403 until ADR 0017 ships.

**Labels after S6:**
- `IMPLEMENTED`: the mechanism
- `BLOCKED` (legal content, OQ-2): jurisdiction values
- `PROVIDER DEPENDENT`: Person matching and in-game reality checks
- `NOT IMPLEMENTED`: staff increases/removals (blocked on ADR 0017)

### 9. Owner / legal questions — orchestrator defaults (reversible by owner/legal)

- **OQ-1 (owner + compliance). DEFAULT: fail closed.**
  - With no RG config in force, deposits and new wagers (and launches) are refused.
  - Withdrawals, settlements and protective actions stay allowed.
  - Reversal path: owner/legal may choose permissive-with-alert. That change is
    confined to one branch in §3.1 step 2.
- **OQ-2 (legal/compliance). DEFAULT: compliance supplies the Anjouan values.**
  - For each §2.1 field, compliance supplies a value or an attested `none`, through
    the floor change-request flow with a named signatory in `attestation_ref`.
  - Engineering supplies **no** values.
  - Until then, Anjouan is `rg_config_missing`.
- **OQ-3 (owner). DEFAULT: limits stop at the operator boundary.**
  - Money and session limits stop at one Person within one operator (tenant).
  - Only self-exclusion and time-out cross operators.
  - Reversal would need cross-tenant ledger reads and data-sharing authority that
    ADR 0006 does not grant, and would require a new ADR.

## Consequences

- One enforcement boundary, no new hot-path lock, and counter-free ledger-derived
  usage (cannot drift). This depends on the new `ledger_entries` index; without it
  every bet scans the player's whole history.
- Under the OQ-1 default, every brand is non-transactable until compliance supplies
  configuration. This is deliberate: the alternative is an implied legal default.
- `player_restrictions` grows a second type (anticipated by ADR 0026 §15).
- ADR 0082 gains an amendment note for the RG writers and the config advisory. ADR
  0027's Uncertain semantics are unchanged; this ADR conforms to them.

## Review disposition (revision 2)

| Review item | Disposition | Where |
|---|---|---|
| LF BLOCKER-1: no index serves `cumulative.go:317` shape; whole-history scan | Accepted. Non-blocking `ledger_entries (tenant_id, player_account_id, asset_code, created_at)` in S1; single ANY+FILTER scan; EXPLAIN gate before S4 | §2.6, §3.4, §8 |
| LF BLOCKER-2: risk specs not reusable (win rollback, bonus legs, no win spec) | Accepted. New RG-only specs by ledger-finance; risk specs unchanged | §1.4, §3.4 |
| LF: reversal only within same window | Accepted | §1.4 |
| LF: negative net loss is an attested field, floor 0 | Accepted (`loss_may_go_negative`) | §1.4, §2.1 |
| LF: SQL window starts, hour intervals, `date_trunc … AT TIME ZONE` | Accepted | §1.2 |
| LF: refuse RR/SERIALIZABLE; post-lock READ COMMITTED read | Accepted | §3.1, §7 |
| LF: deposit usage statuses; no `expired`; TTL sweep; T2 same-tx decline; intent-less deposits; late success credited + alert | Accepted | §1.4, §3.2 |
| LF: `rgfix` permissive synthetic floor; fail-closed in dedicated tests | Accepted | §6 |
| LF: RG writers L0.4 only; static lock-order suite | Accepted | §3.3 |
| LF: no counters; rollups under hourly drift job | Accepted | §1.4 |
| LF: denial audit replay inputs; daily credited-vs-ledger check | Accepted | §4 |
| LF: time-out never changes wallet status (test) | Accepted | §2.3, §7 |
| LF: S3 reports L0.4 hold time | Accepted | §3.4 |
| SEC 1: widen-only for protective options; UNION/OR composition | Accepted | §2.1 |
| SEC 2: `session_refresh` evaluates deadline only, never refuses login | Accepted | §3.1 |
| SEC 3: trigger-set timestamps; one SQL function; `seq` tiebreaker | Accepted | §2.2 |
| SEC 4: step-up 403; player confirm for staff increases; self-target refused; history + notification; decreases no step-up | Accepted | §2.2, §4, §5 |
| SEC 5: MIN across account and person scopes | Accepted | §2.2 |
| SEC 6: lock/statement timeouts fail closed; rate limits; pending cap | Accepted | §2.2, §3.3, §3.5 |
| SEC 7: Match-only person-level increases; notify linked; Uncertain no privileges (ADR 0027 conflict fixed) | Accepted | §2.5 |
| SEC 8: synthetic rows absent outside dev/test; DB trigger on `app.environment` | Accepted (GUC is new in S1) | §2.1 |
| SEC 9: DB-enforced floor four-eyes; distinct persons; hash-bound proof; operator rows not backdatable | Accepted | §2.1 |
| SEC 10: redacted cross-tenant disclosure; RLS extension for time-out; history never leaves tenant | Accepted | §2.2, §2.3, §4 |
| SEC LOW: `post_session_break`, `concurrent_chain_aggregation` | Accepted | §1.5, §2.1 |
| SEC LOW: protective writes bypass `EvaluateAction` | Accepted | §3.1 |
| SEC LOW: unconfigured time-out scope = `platform_person` | Accepted | §1.1 |
| SEC LOW: `count_bonus_funded=false` excludes bonus wins/returns; conversion never reduces loss | Accepted | §1.4 |
| SEC LOW: tenant_person deposit usage joined by person | Accepted | §1.4 |
| SEC LOW: ceiling raise delayed by player cooldown | Accepted | §2.2 |
| SEC LOW: idempotency key reuse with different body = 409 | Accepted | §2.2, §5 |
| OQ-1/2/3 | Recorded as orchestrator defaults, reversible by owner/legal | §9 |

## S1 implementation record (migration 0135; not a status change)

Recorded from the S1 security and ledger-finance reviews. Labels stay as in the Status section
until S1-S6 merge.

- **Implemented differently from the text above, deliberately:**
  - Index build: see the §2.6 amendment.
  - Environment control: `app.environment` is a narrowing input only. The binding control is
    the owner-written, write-once `database_environment` anchor (no row = production). A
    production `platform-api` refuses a `development` / `test` anchor. Recording `production`
    is a required provisioning step.
  - Config key: `rg_limit_configs` carries `asset_code` in its key. NULL means all assets, and
    a numeric ceiling needs an asset.
  - Change requests: the floor change request stores `proposed_config`, and its
    `payload_hash` is derived by the database.
  - `attestation_ref` is restricted to printable ASCII.
  - Read primitives (`compose` / `realized` / `classify`) raise `RL026` for a tenant other
    than the session's.
  - A restrictive policy narrows platform-scope `time_out` visibility to tenants where the
    Person holds an account.
  - Account-scope `time_out` is not yet encoded (S2).
- **S2 must (ledger-finance LF 7):**
  - reject a player value above the composed `ceiling`;
  - reject a `period_kind` outside the composed `allowed_periods` and an `asset_code` /
    `scope` the configuration does not offer;
  - enforce the pending-change cap and map an idempotency-key reuse with a different body to
    409.

  The database does none of these in S1. The S1 trigger classifies and times the change; the
  ceiling clamp is read-time (§2.2).
- **S4 must (ledger-finance LF 8):** run its fail-closed (`rg_config_missing`) tests in a
  jurisdiction that `internal/testsupport/rgfix` has NOT seeded. rgfix seeds a permissive
  synthetic floor for every jurisdiction present when it runs, so a fail-closed test in a
  seeded jurisdiction would pass vacuously.
- **Launch-gate items (security re-review; they do not block S1):**
  - **DONE (IMPLEMENTED, 2026-10-13):** `platform-api` REFUSES to start in production while no
    environment anchor is recorded (`db.ErrEnvironmentAnchorUnrecorded`, from
    `db.VerifyEnvironmentAnchor`; main.go returns the error, no warn path). It also refuses a
    `development` / `test` anchor and an unreadable anchor; non-production guards are unchanged.
    Tests: unit (all anchor states x guards, read error), integration against the real table
    through the runtime role (`internal/db/environment_anchor_integration_test.go`, private DB
    only), and a main.go call-site AST test. Provisioning step: production-configuration-checklist
    ("Database environment anchor"). No migration.
  - Re-run the `attestation_ref` sweep (ASCII and "synthetic" detection) under the production
    database collation before launch.
  - `RETURNING seq` on `player_rg_limit_changes` exposes a global sequence value. This is
    accepted as low risk: it reveals ordering and volume, not content.
- **Security L2 decision:** a player-session insert into `player_restrictions` gets a
  database-chosen `id` (trigger `player_restrictions_player_id_forced`, migration 0135).
  `rg.insertRestriction` reads the stored id back with `RETURNING`. Staff and system inserts
  are unchanged.
- **Application-enforced:** `PermRGConfigWrite` and the staff limit permissions are HTTP-layer
  checks. The database binds operator rows to a same-tenant principal and to tighten-only, not
  to a permission.
