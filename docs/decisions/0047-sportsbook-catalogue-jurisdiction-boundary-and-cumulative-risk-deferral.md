# ADR 0047 — Sportsbook Catalogue/Jurisdiction Boundary and Cumulative Risk: Current State and Deferred Scope

Status: Accepted (documentation of an existing boundary — no new policy, no
human decision answered, no code behavior changed by this document).

Context: Stage 6.1 (B2C/Sportsbook Hardening & Architectural Closure Gate)
required a formal disposition of where sportsbook catalogue exposure and
cumulative risk currently stand, without reopening Stage 4I's jurisdiction
architecture, without answering HDR-J-6/7/8/9, and without inventing
country policy. This ADR records that disposition so it is not re-derived
or silently forgotten in a future stage.

## 1. The six separate concepts, and what Stage 6 actually wired

Per the Stage 6.1 directive, these are distinct concepts and must not be
conflated:

| Concept | Mechanism | Wired for sportsbook? |
|---|---|---|
| A) Player jurisdiction determination | `internal/jurisdiction.DeterminePlayerJurisdiction` / `.Resolve` | No for sportsbook. Called today by `internal/casino` (launch time) and `internal/bonus/eligibility.go` (grant eligibility) — not by `internal/sportsbook`. |
| B) Licence ceiling | `internal/jurisdiction`'s licensing-mode/jurisdiction-ceiling checks, consulted by `jurisdiction.Resolve` | No — inherits (A)'s non-wiring for sportsbook. |
| C) Tenant operating-country policy | `internal/operatingmarket` (Stage 4I Phase E) | Not consulted by sportsbook. Not consulted by any bet/launch path today outside its own admin API. |
| D) Brand operating-country policy | same mechanism as (C), brand-scoped | Not consulted by sportsbook. |
| E) Operation/product availability (risk) | `internal/risk.Evaluate` with `Operation: OperationSportsbookBet` | **Yes** — every bet placement calls `risk.Evaluate` per-request (max/min amount, hard/soft limits, tenant/brand/player/asset/licensing-mode scoping). This is real, tested, enforced. Note: unlike casino, sportsbook's `RiskRequest` does not set `JurisdictionCode` — a rule scoped to a jurisdiction would fail closed with `ErrMissingJurisdiction` (a clean rejection, not a bypass) rather than being evaluated, so this is safe today but is itself a smaller instance of the same wiring gap as (A). |
| F) Sportsbook catalogue availability (per-selection/event blocklist) | would need a new column/mechanism | **No** — `sb_events`/`sb_selections` (migration 0078) carry no jurisdiction-blocklist-equivalent column at all. |
| G) Asset/product eligibility | `assetregistry.AssetAuthorization.CheckEligibility(product, operation)` — its `product` axis was added specifically with sportsbook in mind | Not called by `PlaceBet`. Pre-existing gap shared with casino (only `internal/bonus` calls it today) — not a Stage 6 regression, recorded here for completeness. |

## 2. Comparison with casino, and the real (but currently inert) parity gap

`internal/casino` DOES wire (A) and a per-game blocklist check
(`casino_games.jurisdiction_blocklist`, evaluated via
`evaluateJurisdictionBlocklist` inside `LaunchGame`) — this is real,
tested production code, not a stub. Stage 6's own orchestrator.go
originally claimed "no jurisdiction-blocklist-style check is applied to
a sportsbook bet this stage, exactly as none is applied by any other
production code path today" — **this claim was factually wrong** and has
been corrected in the code comment as part of Stage 6.1. Casino's
mechanism exists and runs on every launch.

However, this is **not a live fail-open regression today**: every
`casino_games.jurisdiction_blocklist` row in this codebase is currently
`'{}'` (the column's own default), because no HDR-J item has been
answered and no country policy has been authorized. Casino's blocklist
check is therefore "armed but empty" right now — it runs, but blocks
nothing, for any game, in any jurisdiction. Sportsbook's behavior (no
check at all) is observably identical in practice today.

Correction (Stage 6.1 architect review): an earlier draft of this ADR
claimed no production configuration path could populate
`jurisdiction_blocklist` at all. That is wrong - `PUT /v1/admin/casino/
games` already lets a platform_admin populate a game's blocklist today,
audited, with no code change and no HDR-J answer required. This makes
the asymmetry sharper, not just theoretical: an ordinary, already-shipped
admin action can arm casino's per-game blocklist right now, while
sportsbook has no equivalent lever to reach for even if an operator
wanted one. The disposition below is unchanged (still safe to defer,
because nothing is actually blocked yet on either side), but the
reasoning is: the gap is one authorized admin action away from being
live for casino and not for sportsbook, not merely "both are equally
inert by construction."

The real gap is architectural symmetry, not live exposure: when a future,
human-authorized country policy DOES get configured, casino already has
a mechanism to enforce it per-game and sportsbook does not.

## 3. Disposition

**Sportsbook catalogue/jurisdiction gating is a MUST FIX BEFORE
PRODUCTION/B2B item, not a MUST FIX BEFORE STAGE 7 item, and definitely
not a MUST FIX NOW item.** Reasoning:
- It is inert today (§2) — there is nothing to block, because no
  jurisdiction/country policy has been authorized anywhere on the
  platform yet.
- The platform currently operates as a single Anjouan-licensed B2C brand.
  A catalogue-level blocklist only matters once a second jurisdiction or
  a B2B tenant with different licensing/geo constraints is onboarded.
- Building it now would require either (a) inventing a blocklist column
  with no real policy to populate it (speculative, explicitly forbidden
  by this stage's directive), or (b) reopening HDR-J-6/7/8/9 (explicitly
  forbidden).
- Deferring it is SAFE precisely because casino's own identical mechanism
  is *also* unarmed — there is no double standard being introduced.

**Required before a second jurisdiction or the first B2B tenant is
onboarded:** add a `jurisdiction_blocklist`-equivalent mechanism to
`sb_events` and/or `sb_selections` (mirroring `casino_games`'s column
and `evaluateJurisdictionBlocklist`'s shape exactly), and wire
`jurisdiction.Resolve` into `PlaceBet` the same way `LaunchGame` already
does. This is additive (a new nullable/defaulted column plus a new
orchestrator check), not a redesign, and does not itself require
answering any HDR-J item — it only needs to exist as a mechanism before
the first real blocklist entry is ever configured.

## 4. Cumulative (rolling-window) risk for sportsbook

`internal/risk`'s per-request evaluation (transaction-level max/min
amount, hard/soft limits) is fully wired for sportsbook (`OperationSportsbookBet`
has been in `internal/risk/types.go`'s operation enum since before Stage 6
and is called on every `PlaceBet`). What is NOT wired is
`operationCumulativeSpecs[OperationSportsbookBet]` — a rolling-window
aggregate-exposure rule (e.g. "no more than X staked across all bets in
one hour") cannot currently be configured for sportsbook; if one is, it
fails closed with `ErrUnsupportedCumulativeOperation` (never silently
"no usage yet" — ADR 0031 §33's own fail-closed guarantee holds).

The exact measurement shape this future wiring needs is **already
specified**, not an open design question: ADR 0038 §13 documents it as
`{TransactionTypes: [sportsbook_bet], ReversalTypes: [sportsbook_void],
MeasuredAccountTypes: [player_cash], ignored: [player_locked_cash],
consuming direction: debit}` — structurally identical to casino's own
already-working `operationCumulativeSpecs[OperationCasinoBet]` entry.
Wiring it is a single additive map entry plus whatever new
`risk_rules` a tenant chooses to configure — no interface change, no
redesign, no new human decision.

**Is current sportsbook risk behavior safe without cumulative exposure?**
Yes. Per-bet risk (max/min amount, hard/soft limits, tenant/brand/
player/asset/licensing-mode scoping) and RG eligibility (self-exclusion,
a `pg_advisory_xact_lock` keyed on person_id closing the concurrent-
self-exclusion race) are both enforced on every single bet, fail-closed
on any risk-evaluation error. What is absent is only an aggregate cap
across MULTIPLE bets in a
time window — a materially different, additive capability, not a gap in
per-bet safety. Two bets placed concurrently cannot "bypass" a
cumulative limit today for the simple reason that no cumulative limit
can be configured for sportsbook at all yet; once one can be, the same
lock-based concurrency discipline `internal/risk`'s cumulative-usage
query already uses for casino (verified race-safe by
`internal/risk/cumulative_race_integration_test.go`) applies unchanged,
since it operates on the ledger rows sportsbook already posts through
the shared `ledger.Post` path.

**Disposition: SAFE DEFERMENT.** No code change this stage. When a
tenant's risk/ledger-finance owner decides sportsbook needs a cumulative
cap, wiring it is a single additive `operationCumulativeSpecs` entry per
the shape already specified in ADR 0038 §13, not a new design.

## 5. Additional deferred items found by Stage 6.1 review (recorded, not fixed)

- **Cumulative-rule write-time validation gap** (security review): `POST
  .../risk/rules` accepts a `cumulative_amount` rule for
  `Operation: sportsbook_bet` (and for other operations with a live
  `risk.Evaluate` call site but no `operationCumulativeSpecs` entry, e.g.
  `casino_launch`/`bonus_grant` — a pre-existing class, not new to
  sportsbook) even though evaluating it will fail closed with
  `ErrUnsupportedCumulativeOperation` on every subsequent bet for that
  tenant. This fails SAFE (no money is ever at risk — the bet is refused,
  not silently accepted), but it is a self-inflicted outage lever any
  `tenant_admin`/`risk_manager` can pull by configuring a rule the system
  cannot evaluate. **Disposition: SAFE DEFERMENT** (no financial-integrity
  impact; a real fix belongs in `risk.CreateRule` validating operation/
  limit-kind support at write time, which is a general `internal/risk`
  robustness improvement affecting every operation, not a sportsbook-
  specific change, and out of this stage's scope).
- **No explicit `PrincipalType == player` assertion** on
  `/v1/me/sportsbook/bets` (security review): safe today only because a
  staff subject's UUID will not resolve via
  `identity.GetPlayerAccountByID` (404), matching casino's identical
  shape. **Disposition: SAFE DEFERMENT** (consistency/defense-in-depth
  item, not a Stage 6 regression).
- **`getSelectionWithContext`'s read is not `FOR SHARE`** (security
  review): inert today because the only writer to `sb_selections` is
  `SyncCatalogue`, run once at server startup, with no live odds-update
  admin endpoint. **Disposition: SAFE DEFERMENT, required before any
  live-odds/real-provider feed** that could update a selection's odds
  concurrently with an in-flight bet placement.

## 6. What this ADR does NOT do

It does not implement a jurisdiction blocklist for sportsbook. It does
not implement cumulative risk for sportsbook. It does not answer
HDR-J-6/7/8/9. It does not seed or invent any country policy. It records
the current boundary and the two additive, well-specified future tasks
identified above, for `docs/governance/task-registry.md` to track.
