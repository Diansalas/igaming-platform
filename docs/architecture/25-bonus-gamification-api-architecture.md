# 25 — Bonus/Gamification API Architecture (Forward-Looking Contract Sketch)

Status: Stage 4H-A. **ARCHITECTURE-FREEZE-ONLY.**

> **This document is a forward-looking contract sketch for a domain that is
> not yet implemented.** It modifies no code and no route table. It does
> **not** change `docs/api/openapi/platform-api.yaml` — that file remains
> the real, live spec for already-implemented endpoints, and stays
> untouched by this stage. It creates no new HTTP routes and no new Go
> types. The resource shapes below are conceptual (what fields exist and
> what they mean), not JSON Schema, and are deliberately not pinned to any
> other specialist's in-flight internal data model (Reward Orchestrator,
> canonical event taxonomy, External Reward Provider contract, Points
> accounting, Bonus Accounting ADR, Gamification/Tournament/Mission/
> Marketplace architecture — all owned elsewhere, in parallel, this
> stage). When Bonus/Gamification is actually implemented in a future
> stage, the OpenAPI schema additions happen then, against whatever this
> contract has by that point been reconciled with the finished
> architecture docs. Nothing here is a commitment that any of these
> endpoints ship in the next stage.

This document restates and applies this codebase's *existing* API
conventions (`docs/architecture/04-api-architecture.md`,
`internal/apierror`, `internal/httpserver/risk_handlers.go`,
`internal/httpserver/wallet_handlers.go`, `GET /v1/me`) to a new domain. It
invents no new error shape, no new auth model, and no new tenant-isolation
mechanism.

## 0. Ground rules (inherited, not new)

- **Tenant resolution is always server-side.** Every endpoint below
  resolves `tenant_id` (and, where relevant, `brand_id`) from the
  authenticated context (`tenant.FromContext`, same as every existing
  handler) — never from a path/query/body parameter. A client-supplied
  tenant id, if one is ever accepted as a filter for a staff/admin list
  endpoint, is validated against the caller's own tenant scope and never
  trusted to select a different tenant's data. This is CLAUDE.md's
  absolute rule, restated, not renegotiated by this document.
- **Error shape.** Every endpoint reuses `internal/apierror.Error`
  (`code`, `message`, `request_id`) and the existing `Code` enum
  (`validation_error`, `unauthorized`, `forbidden`, `not_found`,
  `conflict`, `internal_error`, `service_unavailable`,
  `tenant_mismatch`, `rate_limited`). No new error taxonomy is introduced
  for this domain. Domain-specific failure detail (e.g. "mission already
  completed", "insufficient points balance", "tournament entry window
  closed") is carried in `message` under an existing `Code`
  (`validation_error` or `conflict` as appropriate), exactly as
  `risk.ErrInvalidInput`/`risk.ErrNotFound` map onto `CodeValidation`/
  `CodeNotFound` today. If a future implementation finds it needs a
  genuinely new machine-readable code, that is a proposal to `architect`
  against `internal/apierror`, not a domain-local invention.
- **RBAC enforcement plumbing.** Player-initiated endpoints authenticate a
  player JWT exactly as `GET /v1/me`/`GET /v1/wallets` do today (subject =
  player_account id, scoped by tenant). Staff/admin endpoints authenticate
  via the existing staff-auth path and are gated by permission constants
  in `internal/auth/permission.go`, following the `risk_config:read` /
  `risk_config:manage` read/manage split exactly (see §3). This document
  proposes permission *names* only — no role-permission wiring, no new
  `Permission` constants committed to code, no route table changes.
- **Idempotency.** Every proposed mutating admin endpoint (create-style)
  follows the existing `risk_rules` precedent: creation is either (a)
  naturally append-only with no update/delete semantics at all (mirroring
  `POST /v1/admin/risk/rules`, which only ever adds a row — "disable" is
  itself a new state transition, never a delete), or (b) idempotent-safe
  via a caller-supplied client reference honored by a DB-enforced unique
  constraint — mirroring the ledger's `(provider_id, provider_tx_id)`
  pattern (CLAUDE.md, "financial writes") generalized to non-financial
  writes as `(tenant_id, resource_type, client_reference)`. No admin
  mutation in this domain is a "check-then-insert" in application code.
  Player-initiated mutations that are inherently one-shot per player
  (e.g. "opt into mission X", "claim reward Y") are idempotent by being
  naturally keyed on `(player_account_id, mission_id)` /
  `(player_account_id, reward_id)` with a DB unique constraint — a second
  identical request returns the existing state (200/409, not a duplicate
  row), never silently double-applies.
- **Pagination.** No concrete pagination convention exists yet anywhere in
  this codebase's *implemented* endpoints — every current list endpoint
  (`GET /v1/wallets`, `GET /v1/admin/risk/rules`, `GET
  /v1/admin/audit-log`) returns a full array or filters down to a scoped
  set, and `04-api-architecture.md` only records pagination as an
  unconfirmed `RECOMMENDATION`. This domain is the first to need it for
  real (leaderboards, mission catalogues, marketplace catalogues,
  points/reward history can all be unbounded), so this document makes a
  concrete recommendation for `architect` to ratify or amend at
  implementation time rather than leaving every list endpoint to invent
  its own shape:
  - Cursor-based pagination for anything ordered by time or rank
    (points history, reward history, leaderboard standings):
    `?limit=<n>&cursor=<opaque>`, response envelope
    `{ "items": [...], "next_cursor": "<opaque|null>" }`. Cursor-based
    (not offset) because leaderboard/points-ledger rows are actively
    appended to while a staff member or player is paging, and offset
    pagination skips/duplicates rows under concurrent writes.
  - Simple `limit`/`offset` remains acceptable for genuinely static,
    small, operator-curated lists (mission catalogue, marketplace
    catalogue, badge catalogue) where the total count is small and bounded
    by tenant configuration, not by player activity volume.
  - `limit` is server-capped (proposed default 50, max 200) regardless of
    what the caller requests, mirroring the general principle that
    back-office/partner-console screens must never be able to force an
    unbounded query (`04-api-architecture.md`, "Blueprint §7").
- **Versioning.** All endpoints below are proposed under the existing
  `/v1/` prefix (`/v1/me/...` for player-facing, `/v1/admin/...` for
  staff-facing), not a separate version namespace. Rationale: this
  codebase versions the whole platform API as one contract (`/v1/auth`,
  `/v1/me`, `/v1/admin/...` all share one prefix today; there is no
  per-domain versioning precedent anywhere in the codebase), and B2B
  partners integrating against `/v1` need one stable contract surface, not
  N per-domain version clocks. The concern motivating "should fast-moving
  gamification get its own version" is real — mission/tournament/
  marketplace shapes are far more likely to churn early than identity or
  ledger — but the answer this document recommends is **field-level
  additive evolution within `/v1`, not a separate version prefix**:
  gamification resources should be designed so new seasons/mission types/
  reward kinds are additive (new enum values, new optional fields, new
  catalogue rows) rather than breaking changes, exactly as `risk_rules`
  added `licensing_mode`/jurisdiction scoping without a version bump. If
  and when a genuinely breaking reshape becomes unavoidable, that is a
  `/v2` decision for `architect`, scoped to the whole API, not a
  domain-local `/v1-gamification`. **Recommendation, not yet ratified** —
  flagged explicitly for `architect` to confirm or override before any
  implementation stage begins.

## 1. Resource areas

Each area lists: conceptual shape, player vs. staff operations, tenant/
brand scoping, and idempotency notes. None of this is a route table —
paths shown are illustrative of the convention, not a commitment.

### 1.1 Player bonus wallet

Conceptual shape: a player's bonus-side view derived from the existing
wallet/ledger split (`06-wallet-ledger-architecture.md`'s `player_bonus`
balance, plus the not-yet-frozen Bonus Accounting model) — active bonus
grants, each with: offer reference, granted amount, remaining wagering
requirement, contribution rules summary, expiry, status
(active/completed/forfeited/expired). This resource **reads** the ledger's
bonus balance and the bonus engine's grant/progress state; it never
performs a balance mutation itself (`ledger-finance` owns all balance
mutation, per CLAUDE.md — this API surface is read/orchestration-trigger
only).

- Player: `GET /v1/me/bonuses` (list own active + historical grants),
  `GET /v1/me/bonuses/{grantID}` (detail + wagering progress).
- Staff: `GET /v1/admin/players/{id}/bonuses` (support/compliance
  visibility into one player's bonus history — mirrors
  `GET /v1/admin/players/{id}` precedent), gated by a proposed
  `bonus:read` permission (player-scoped read, distinct from
  `bonus_config:read`/`bonus_config:manage` for campaign/offer authoring —
  mirrors `player:read` vs. `risk_config:read` being separate authorities
  today). **Specialist-review correction (P1 fix, F3)**: an earlier draft
  of this document referenced `bonus_config:read`/`bonus_config:manage`
  in passing (as here) but never actually defined them as a resource area
  or listed them in §2's permission summary — the absence, not a typo, was
  the finding. Bonus campaign/Offer authoring is the highest-value
  configuration surface in this entire domain (an Offer with a narrow
  eligibility axis and a large reward is functionally equivalent to a
  manual grant, without carrying a manual grant's four-eyes/threshold
  control — `docs/architecture/10-bonus-engine-architecture.md` §10 only
  requires that treatment for genuinely manual overrides, not for
  authoring an Offer that makes the grant automatic). This document now
  requires: (i) `bonus_config:read`/`bonus_config:manage` as a first-class
  permission pair (added to §2 below), never bundled into
  `RoleTenantAdmin` by default; (ii) any Offer whose maximum per-player
  reward value exceeds a configurable threshold requires the same
  reason-code + four-eyes treatment as a manual adjustment, closing the
  "configure it instead of adjusting it" bypass.
- **Specialist-review addition (P2 fix, F15)**: `awaiting_verification`
  (a KYC-gated grant status, per
  `docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md`
  §6) is a KYC-derived fact. A `bonus:read` holder without the separate
  `verification:read` permission must not be able to infer a player's KYC
  state from this endpoint — either the status is generalized to a
  non-KYC-specific "pending" value for a caller lacking
  `verification:read`, or `bonus:read` is defined to always require
  `verification:read` alongside it. `bonus:read` must never become a
  side-channel around `verification:read`'s deliberate separation of
  duties (`internal/auth/permission.go`'s explicit "never grant sensitive
  verification access to Finance" precedent, applied by analogy).
- No player-initiated mutation here (claiming/opting into an *offer* is a
  missions/marketplace-style action, see 1.2/1.8); forfeiture/cancellation
  is a staff or system (bonus-engine) action, not exposed as a raw player
  mutation.
- Tenant scoping: identical pattern to `GET /v1/wallets` — resolved
  player_account id from JWT subject, tenant from JWT, brand resolved
  server-side via the player's own account, never client-supplied.

### 1.2 Missions

Conceptual shape: a mission definition (name, description, objective type,
target, reward on completion, availability window, per-tenant/brand
configuration) plus a player's progress against it (current value, target,
status: available/in_progress/completed/expired, opted_in boolean if
opt-in is required).

- Player: `GET /v1/me/missions` (available + in-progress + completed,
  paginated per §0), `GET /v1/me/missions/{id}` (detail + progress),
  `POST /v1/me/missions/{id}/opt-in` (player-initiated opt-in, idempotent
  on `(player_account_id, mission_id)` — a repeat call returns the
  existing opted-in state, not a duplicate enrollment or an error).
- Staff: `GET /v1/admin/missions` (catalogue, all definitions for the
  tenant/brand), `POST /v1/admin/missions` (create a mission definition —
  idempotent via a caller-supplied `client_reference`, mirroring the
  risk-rule-creation pattern), `POST /v1/admin/missions/{id}/disable`
  (mirrors `risk_rules`' disable-not-delete pattern — append-only state
  transition, never a row deletion).
- Permissions: `mission_config:read` / `mission_config:manage` for the
  admin catalogue surface, mirroring `risk_config:read/manage` exactly.
  Player opt-in/progress endpoints require only standard player
  authentication, no staff permission.
- Tenant/brand scoping: mission *definitions* are tenant+brand-scoped
  configuration rows (per CLAUDE.md's "brand differences are configuration
  rows" rule) — never a code path keyed on which brand is asking.

### 1.3 Levels

Conceptual shape: a tenant-configured level ladder (ordered levels, each
with an XP/points threshold and level-up rewards) and a player's current
position (current level, XP/points toward next threshold, next level's
threshold).

- Player: `GET /v1/me/level` (current level + progress + next threshold).
  No player-initiated mutation — level-up is a derived, system-computed
  state transition from XP/points accrual (owned by the Reward
  Orchestrator / points-accounting side, not a player action).
- Staff: `GET /v1/admin/levels` (the configured ladder for a tenant/
  brand), `PUT /v1/admin/levels` or `POST /v1/admin/levels` (define/
  replace the ladder — idempotent as a full-ladder replace keyed on
  `(tenant_id, brand_id)`, since a "level ladder" is inherently a single
  versioned configuration object per brand, not an appendable list of
  independent rows; each replace is itself an audited, versioned write,
  never a silent overwrite — mirrors CLAUDE.md's "versioned and editable"
  rule for brand configuration).
- Permissions: `gamification_config:read` / `gamification_config:manage`
  (levels are tenant-wide gamification configuration, not their own
  narrow permission — there is no separation-of-duties reason to split
  level-ladder authority from the general gamification-configuration
  authority the way risk-limit-writing is deliberately split from
  risk-limit-reading).

### 1.4 Points

Conceptual shape: mirrors this codebase's existing wallet balance/history
split (`wallet.Summary` + a transaction history endpoint), but against the
**separate points ledger** (owned by `ledger-finance`'s Points accounting
work, not the money wallet) — a non-monetary, tenant-scoped balance plus
an append-only history of point-earning/point-spending events (source
type, amount, resulting balance, timestamp, reference to the triggering
event e.g. bet settlement, mission completion, marketplace redemption).

- Player: `GET /v1/me/points` (current balance, mirroring
  `walletSummaryResponse`'s shape but for points, not minor-unit money),
  `GET /v1/me/points/history` (paginated, cursor-based per §0 — this is
  exactly the kind of append-only, high-volume, time-ordered list cursor
  pagination is for).
- Staff: `GET /v1/admin/players/{id}/points` and
  `GET /v1/admin/players/{id}/points/history` (support/compliance
  visibility, gated by `bonus:read` or a dedicated `points:read` —
  left to `architect`/`ledger-finance` to decide whether points visibility
  bundles with bonus visibility or stands alone, since points-ledger
  ownership is being defined concurrently in the Points Accounting ADR
  this stage).
- No direct player or staff mutation of a points balance through this API
  — exactly as the money wallet is never mutated by direct API write
  (CLAUDE.md: "never implements financial balance mutations directly").
  Every points change flows through the points ledger's own posting
  mechanism (owned by `ledger-finance`), triggered by domain events
  (bet settled, mission completed, marketplace redemption), never by a
  `PATCH /v1/me/points`-style endpoint.

### 1.5 Badges

Conceptual shape: a badge definition (name, description, criteria summary,
icon reference) and a player's earned badges (badge reference, earned_at
timestamp). Read-only, small, catalogue-like.

- Player: `GET /v1/me/badges` (earned), `GET /v1/badges` or
  `GET /v1/me/badges/available` (catalogue of badges not yet earned, for
  a "what can I still earn" screen — tenant/brand-scoped catalogue).
- Staff: `GET /v1/admin/badges` (catalogue management read),
  `POST /v1/admin/badges` (define a badge — idempotent via
  `client_reference`, append-only, no update path this stage, mirroring
  `risk_rules`' minimal-write-surface precedent).
- Badges are awarded by system/event logic (Reward Orchestrator), never by
  a direct staff "grant badge to player" mutation in this initial
  contract sketch — if a manual-grant admin path is needed later (e.g.
  goodwill/support case), it is a `POST /v1/admin/players/{id}/badges`
  action requiring a reason code and audit record, following CLAUDE.md's
  "every mutating administrative action writes an audit record" rule; not
  designed in detail here since it wasn't in the requested scope.
- Permissions: `gamification_config:read` / `gamification_config:manage`
  for the catalogue; earned-badge visibility for a specific player is
  `player:read`-adjacent (mirrors how `GET /v1/admin/players/{id}` already
  aggregates player-scoped read data).

### 1.6 Tournaments

Conceptual shape: a tournament definition (name, game/product scope,
entry criteria, schedule window, prize structure) and its live state
(current leaderboard position for a player, overall standings, results
once concluded).

- Player: `GET /v1/me/tournaments` (available + entered), `GET
  /v1/tournaments/{id}` (detail), `POST /v1/tournaments/{id}/enter`
  (player-initiated entry — idempotent on `(player_account_id,
  tournament_id)`, a repeat call is a no-op returning existing entry
  state, never a duplicate entry or a double-charged entry fee if entry
  fees exist — entry-fee handling itself is a `ledger-finance` /
  wallet-service concern, this API only orchestrates the entry record),
  `GET /v1/tournaments/{id}/leaderboard` (current standings, cursor- or
  rank-window-paginated per §0).
- Staff: `GET /v1/admin/tournaments`, `POST /v1/admin/tournaments`
  (create — idempotent via `client_reference`, append-only definition),
  `POST /v1/admin/tournaments/{id}/close` or `/settle` (state transition
  to concluded/settled — mirrors the withdrawal state-machine precedent
  of explicit, audited state transitions rather than field overwrites),
  `GET /v1/admin/tournaments/{id}/results`.
- Permissions: `tournament_config:read` / `tournament_config:manage` for
  DEFINITION authoring (name, schedule, prize structure), mirroring
  `risk_config:read/manage` exactly. **Specialist-review correction (P1
  fix, F2)**: an earlier draft placed `/close` and `/settle` under this
  SAME `tournament_config:manage` permission — this is the exact
  anti-pattern `internal/auth/permission.go` already solved twice for
  withdrawal policy and risk config ("the role that approves withdrawals
  must not also be the role that can loosen the policy gating its own
  approvals"), applied here: a single actor holding
  `tournament_config:manage` could author a tournament's prize structure
  AND settle it themselves, with no second pair of eyes on a transition
  that pays out real money. Settlement/close/recalculation authority is
  its own permission, **`tournament:settle`**, never bundled with
  `tournament_config:manage` by default and never granted to the same
  role as a matter of course.
- Tenant/brand scoping: a tournament is tenant+brand-scoped configuration;
  a player can only see/enter tournaments scoped to their own brand,
  resolved server-side from their account, never from a client-supplied
  brand id.

### 1.7 Leaderboards

Conceptual shape: closely related to tournaments but generalized —
current-period and historical leaderboard snapshots for a given metric
(points earned, wagering volume, tournament score), potentially spanning
multiple concurrent leaderboard "boards" per brand (e.g. weekly points
leaderboard, VIP wagering leaderboard).

- Player: `GET /v1/me/leaderboards/{boardID}/rank` (own current rank +
  score), `GET /v1/leaderboards/{boardID}` (current-period standings,
  cursor-paginated), `GET /v1/leaderboards/{boardID}/history/{periodID}`
  (a concluded period's final standings — immutable once the period
  closes, mirroring the ledger's "never edit history" principle applied
  to a non-financial append-only record). **Specialist-review addition
  (P1 fix, F8)**: both endpoints return the PLAYER-FACING projection only
  — display identity, rank, score — never a withholding/disqualification
  reason for any participant, and never an RG-derived code under any
  circumstance, per `docs/architecture/18-tournament-architecture.md`'s
  corrected two-projection rule (§3). A withheld/disqualified participant
  is either omitted from this response or shown with no reason at all.
  The full internal record (including RG/Risk decision codes) is
  staff/audit-only, exposed through a separate, RG-aware-permission-gated
  endpoint this document does not sketch (deferred — the player-facing
  contract is what must be locked down now, since it is the harder one to
  retrofit).
- Staff: `GET /v1/admin/leaderboards` (definitions), `POST
  /v1/admin/leaderboards` (define a board — idempotent via
  `client_reference`).
- Permissions: `gamification_config:read` / `gamification_config:manage`
  for board definitions; standings themselves are public-to-the-brand's-
  own-players read data, no special permission beyond player auth (a
  leaderboard is inherently a shared view across players of the same
  tenant/brand — the tenant-isolation rule still applies at the
  tenant/brand boundary: a leaderboard never shows or ranks against
  another tenant's players, since `tenant_id`/`brand_id` scope the query
  server-side exactly as everywhere else).

### 1.8 Marketplace

Conceptual shape: a catalogue of redeemable items (name, description,
points cost, stock/availability, category) and a player's redemption
history (item, cost paid, status: pending/fulfilled/failed/refunded,
fulfillment reference — which may point to an externally-fulfilled reward,
see 1.9).

- Player: `GET /v1/me/marketplace/catalog` (available items for their
  tenant/brand, paginated per §0's "static curated list" bucket), `POST
  /v1/me/marketplace/purchases` (redeem an item — this is the one
  player-initiated write in this domain that most resembles a financial
  debit, since it spends points; it must be idempotent via a
  caller-supplied `client_reference` per purchase attempt, exactly
  mirroring how any client-retriable mutating request in this codebase
  is required to behave, and the actual points debit is a `ledger-finance`
  points-ledger posting, never an application-level "check balance then
  deduct" here), `GET /v1/me/marketplace/purchases` (own redemption
  history, cursor-paginated).
- Staff: `GET /v1/admin/marketplace/catalog`, `POST
  /v1/admin/marketplace/catalog` (add/update a catalogue item —
  idempotent via `client_reference`), `GET
  /v1/admin/marketplace/purchases` (support/audit visibility across
  players, cursor-paginated, tenant-scoped).
- Permissions: `marketplace_config:read` / `marketplace_config:manage` for
  catalogue administration; purchase visibility across players is
  `bonus:read`-adjacent/audit-style read, left to `architect` to fold into
  an existing permission family or mint a `marketplace:read` at
  implementation time.
- This is also where the External Reward Provider contract (owned by the
  Master Orchestrator, in progress this stage) matters most: a catalogue
  item's fulfillment may be internal (points→bonus grant) or external
  (points→physical/voucher reward via a provider). This API surface
  exposes only the redemption request and its status, never a
  provider-specific shape — the provider abstraction lives entirely
  behind fulfillment, per this codebase's existing provider-interface
  convention (CLAUDE.md, "Provider abstraction").

### 1.9 Rewards

Conceptual shape: a player's consolidated reward history spanning every
reward-producing source (bonus grant, mission completion, badge award,
tournament prize, marketplace redemption) and every fulfillment path
(internal ledger posting vs. externally-fulfilled via the External Reward
Provider contract), each with: source reference, reward description,
status (pending/fulfilled/failed), fulfillment channel
(internal/external), external provider reference where applicable.

- Player: `GET /v1/me/rewards` (consolidated history, cursor-paginated),
  `GET /v1/me/rewards/{id}` (detail, including external fulfillment
  status if applicable — read-through to the External Reward Provider's
  reported state, never a client-trusted status).
- Staff: `GET /v1/admin/players/{id}/rewards` (support/compliance
  visibility, mirrors 1.1's admin bonus-history precedent), gated by the
  same `bonus:read`-family permission.
- This resource is explicitly a **read-model aggregate** over other
  domains' write paths (bonus, missions, badges, tournaments,
  marketplace) — it has no mutation of its own beyond what already
  happens via those resources' own endpoints, matching how the
  Gamification Profile (1.10) is also a pure read aggregate.

### 1.10 Gamification profile (consolidated "me" view)

Conceptual shape: mirrors this codebase's existing `GET /v1/me` precedent
(a single, authenticated, server-scoped summary aggregate) — a read-model
combining current level + XP/points-toward-next-level, points balance,
badge count/most-recent badges, active mission count/summaries, and
active bonus count/summary. Purely additive to the existing identity `GET
/v1/me` (`internal/httpserver/auth_routes.go`'s `newMeHandler`/
`meResponse`) — this document proposes it as its own endpoint rather than
a field bolted onto identity's `meResponse`, since gamification-profile
composition spans multiple future-owned subsystems (points ledger, bonus
engine, Reward Orchestrator) that identity's `GET /v1/me` handler has no
reason to depend on; keeping them separate avoids coupling the identity
read path to gamification's future data model.

- Player: `GET /v1/me/gamification-profile` — single aggregate read,
  built the same way `newMeHandler` builds `meResponse` today: resolve
  `player_account_id` from the JWT subject, tenant from JWT, then read
  (never mutate) from each contributing subsystem under the caller's own
  tenant scope.
- Staff: `GET /v1/admin/players/{id}/gamification-profile` for
  support/compliance, same permission family as 1.9/1.1.
- No mutation surface — every underlying value is owned and mutated by
  its own resource area (1.2–1.9) or by system/event logic, never by this
  aggregate directly.

## 2. Permission-naming summary (proposal only — no code changes)

Following the `<resource>_config:read` / `<resource>_config:manage`
convention established by `risk_config:read`/`risk_config:manage` and
`casino_config:write`/`withdrawal_policy:write`:

| Proposed permission | Gates |
|---|---|
| `bonus:read` | Staff visibility into a specific player's bonus/points/reward history (1.1, 1.4, 1.9, 1.10) — MUST NOT disclose a KYC-derived status to a caller lacking `verification:read` (F15, §1.1) |
| **`bonus_config:read` / `bonus_config:manage`** | **Specialist-review addition (P1 fix, F3) — previously referenced but never defined.** Campaign/Offer authoring (§1.1) — the highest-value configuration surface in this domain; `manage` never bundled into `RoleTenantAdmin` by default, and an Offer whose max per-player reward exceeds a configurable threshold requires the same four-eyes treatment as a manual adjustment |
| `mission_config:read` / `mission_config:manage` | Mission catalogue visibility / authoring (1.2) |
| `gamification_config:read` / `gamification_config:manage` | Level ladder, badge catalogue, leaderboard definitions (1.3, 1.5, 1.7) |
| `tournament_config:read` / `tournament_config:manage` | Tournament DEFINITION visibility / authoring only (1.6) — does NOT gate settlement |
| **`tournament:settle`** | **Specialist-review addition (P1 fix, F2) — previously bundled into `tournament_config:manage`.** Settlement, close, and recalculation authority. Never bundled with `tournament_config:manage`, never granted to the same role as a matter of course — the role that authors a prize structure must not also be the role that pays it out unaccompanied |
| `marketplace_config:read` / `marketplace_config:manage` | Marketplace catalogue administration (1.8) |

**Specialist-review addition — bundling risk, recorded as a build-time
requirement (F2/F3's underlying structural issue):** every narrow
authority already in this codebase's real permission model has a
DEDICATED role holding it and nothing else (`RoleFinance` for the four
withdrawal permissions, `RoleRiskManager` for the two `risk_config`
permissions, `RoleCompliance` for `rg_restriction:write`/
`identity_review:manage`/`verification:review`). This document proposes
seven `manage`-class permissions (`bonus_config:manage`,
`mission_config:manage`, `gamification_config:manage`,
`tournament_config:manage`, `tournament:settle`,
`marketplace_config:manage`) and names no role to hold them — a
permission with no grantee is a capability nothing can actually use,
which makes bundling it into `RoleTenantAdmin` the path of least
resistance at build time, exactly the failure `internal/auth/
permission.go`'s own `RolePlatformAdmin` RG comment warns against.
**Required at build time**: mint a dedicated role (e.g.
`RolePromotionsManager`) holding this `manage`/`settle` set and nothing
else, mirroring `RoleRiskManager`'s shape exactly — this is a build-time
requirement recorded now, not resolved by this architecture-freeze stage.
The `*_config:read` side may safely bundle into `RoleTenantAdmin`, which
already holds `PermRiskConfigRead` today — that precedent is explicitly
fine to follow.

Left open for `architect`/the eventual implementer to decide at build
time (flagged, not resolved here): whether `bonus:read` should absorb
points/rewards visibility entirely or split further once the Points
Accounting ADR and Reward Orchestrator design land — this document does
not commit to a final permission count, only to the naming convention and
the read/manage separation-of-duties pattern.

## 3. Explicit non-goals of this document

- No Go types, no route registrations, no OpenAPI YAML.
- No decision on the exact points-ledger data model (owned by
  `ledger-finance`, this stage).
- No decision on the Reward Orchestrator's internal event contract or the
  External Reward Provider interface shape (owned by the Master
  Orchestrator, this stage) — this document only assumes such a provider
  abstraction exists and that marketplace/reward fulfillment status is
  read through it, never that a specific field or method exists on it.
- No RBAC role-to-permission wiring (no `rolePermissions` map changes).
- No claim that any endpoint listed here is scheduled for a specific
  future stage — sequencing is the orchestrator's decision, not this
  document's.

## 4. Ownership

Authored by `backend-platform` per Stage 4H-A's assignment. Cross-domain
inputs from `architect` (Gamification/Tournament/Mission/Marketplace
architecture), `bonus-engine` (Bonus Architecture rewrite),
`ledger-finance` (Points accounting, Bonus Accounting ADR), `risk` (ADR
0031 update), `identity-compliance` (RG/KYC ADR), `sportsbook` (Provider
Interoperability ADR), and the Master Orchestrator (Reward Orchestrator,
canonical event taxonomy, External Reward Provider contract) are expected
to be reconciled against this contract sketch before implementation
begins — this document does not supersede or finalize any of them.
