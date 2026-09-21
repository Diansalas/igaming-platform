# 0083 — Sportsbook Jurisdiction/Market Gating and Cumulative Risk & Exposure (Stage 9.2)

## Status

Accepted — **design only**. No Go source file, no `.sql` migration and no
OpenAPI document is changed by this ADR's own dispatch. Implementation is
delegated to a later wave (`sportsbook` with `risk` for Part B, `security`
review mandatory for Part C), who must implement exactly what §5–§9
specify and nothing else. Every architectural invariant in §10 is binding
on `qa` and `code-reviewer`.

**One combined ADR, not two — and why.** Both workstreams terminate in the
same function, `internal/sportsbook.PlaceBet`, and the single most
error-prone part of either design is the *order* in which the new gates
compose with each other and with ADR 0082's canonical lock ordering. Two
ADRs would have to carry two copies of that order, and two copies of an
ordering rule is how ordering rules drift. §7 is therefore the single
authoritative statement of `PlaceBet`'s composed call order, and both
workstreams reference it rather than restating it.

Part B (§6) is cumulative risk and exposure. Part C (§5) is
jurisdiction/market gating. They are separable at implementation time
(§9.4 gives the wave split) but not at design time.

## Relationship to ADR 0047 — PARTIALLY superseded

`docs/decisions/0047-sportsbook-catalogue-jurisdiction-boundary-and-cumulative-risk-deferral.md`
is the baseline for this ADR and is **partially superseded**:

| ADR 0047 element | Disposition here |
| --- | --- |
| §3 disposition "MUST FIX BEFORE PRODUCTION/B2B, not now" (catalogue/jurisdiction) | **Deferral LIFTED.** Superseded by §5. |
| §3 prescribed mechanism: "add a `jurisdiction_blocklist`-equivalent **column** to `sb_events` and/or `sb_selections`, mirroring `casino_games`'s column exactly" | **SUPERSEDED — this prescription has drifted and is now wrong.** Migration 0084 (ADR 0081, landed *after* ADR 0047) made the only write path to the five `sb_*` tables the `app.platform_service_id = 'sportsbook_catalogue_sync'` GUC. A column there would be writable only by the provider-driven catalogue sync. See §5.2 for the five-part argument and the replacement design. |
| §4 disposition "SAFE DEFERMENT" (cumulative risk) | **Deferral LIFTED.** Superseded by §6.1. |
| §4's stated spec shape `{[sportsbook_bet], [sportsbook_void], measured=[player_cash], ignored=[player_locked_cash], debit}` | **PARTIALLY SUPERSEDED.** `measured`/`ignored`/`direction` re-verified correct against HEAD. `ReversalTypes: [sportsbook_void]` is **wrong**: `sportsbook_void` is not an admitted `ledger_transactions.transaction_type` value anywhere (migration 0078's CHECK lists seventeen values and does not include it; the string appears only in two Go comments). Corrected in §6.1. |
| §4's claim that cumulative wiring needs "no new human decision" | **CONFIRMED for the player-scoped rolling-window limit (§6.1).** **Does not hold for cross-player exposure (§6.2)**, which ADR 0047 never analysed and which is a different concept entirely — see HDR-SB-1 (§8). |
| §2's non-negotiable constraints (no invented country policy; no HDR-J answer; casino's K3-1/K3-2 arming semantics; jurisdiction never client-supplied) | **CARRIED FORWARD UNCHANGED and restated as invariants in §10.** |
| §5's four deferred items | Two are **closed** by this ADR (`getSelectionWithContext` not `FOR SHARE` → §5.6; cumulative write-time validation, for `sportsbook_bet` only → §6.1.4). Two remain deferred, unchanged. |

ADR 0047 is not withdrawn: its §1 six-concept table and §2 casino-parity
analysis remain the correct statement of *why* this work exists.

This ADR also **amends ADR 0082** (§7.3, new lock class **L0.6**, new
named exception **E-3**) and **answers `docs/architecture/09-sportsbook-architecture.md`
Open Question 3** (§6.2.1).

## 1. What was verified against HEAD, and what had drifted

Everything below was read from source, not inherited from a prior stage's
characterisation. Four of the prior characterisations were wrong.

1. **`operationCumulativeSpecs[OperationSportsbookBet]` is not "an unwired
   placeholder" — there is no entry at all.** `internal/risk/cumulative.go`
   has exactly two production entries (`OperationCasinoBet`,
   `OperationBonusConversion`). The sportsbook shape exists only as prose
   inside the map's doc comment. Adding it is a genuine new map literal,
   not the un-commenting of a stub.
2. **`player_locked_cash` IS still the correct ignored account type.**
   Re-verified against `internal/ledger/ledger.go`'s `AccountType`
   constants at HEAD (post-migration-0048, post-ADR-0082): the constant
   set is `player_cash`, `player_bonus`, `player_locked_cash`,
   `player_locked_bonus`, `player_withdrawal_hold`, `house_gaming`,
   `provider_payable`, `psp_clearing`, `psp_reserve`,
   `jackpot_contribution`, `promo_liability`, `manual_adjustment`,
   `bonus_expense`, `player_bonus_held`. `PlaceBet` posts
   `Dr player_cash / Cr player_locked_cash`. ADR 0082 changed *lock
   ordering*, not account types.
3. **`sportsbook_void` does not exist.** Not in migration 0078's
   `transaction_type` CHECK, not in any later migration, not in
   `internal/ledger`'s `TxSportsbook*` constants. ADR 0047 §4, doc 09
   §16.3 and `cumulative.go`'s own comment all name it. Corrected in
   §6.1.2.
4. **ADR 0047 §3's prescribed column is no longer implementable as
   written.** Migration 0084 landed after ADR 0047 and changed the
   write-authorization premise the prescription rested on. See §5.2.
5. **`PlaceBet` already composes with ADR 0082 correctly** — the
   `lockCashBalance` helper is gone, `ledger.GetOrCreateAccounts` and
   `ledger.LockProjectionsForPosting` are in place, and the same
   `betInput` value feeds the pre-lock and `ledger.Post` (R3 satisfied).
   This ADR must not disturb any of it.
6. **`PlaceBet` already contains an unnamed ADR 0082 ordering
   exception.** `insertBet` runs *after* `ledger.Post`, so a class **L1**
   `sportsbook_bets` unique-index insertion wait is acquired after L3/L4.
   ADR 0082 §1.5 classified sportsbook bets as L1 but §4.4 never named
   this inversion. Named and guarded as **E-3** in §7.3.
7. **`GET /v1/sportsbook/sports` and `GET /v1/sportsbook/events/{id}` are
   anonymous** (ADR 0081 §2.3 relies on it; the flow tests call them with
   an empty token). There is no player to resolve a jurisdiction for on an
   anonymous read. This constrains §5.4 decisively.
8. **`internal/operatingmarket` still has zero production callers.**
   Verified by import graph: outside its own package and tests, only
   `internal/jurisdiction` (for `EvaluateLicenceValidity`) and
   `internal/db/production_safety.go` reference it.
9. **`jurisdiction.Resolve` with a non-nil `PlayerAccountID` returns
   `unresolved(no_signal)` unconditionally, without touching the
   database.** `resolveTenantLicence` — the only branch that can return
   `Resolved` — is structurally unreachable for a player-scoped call
   (`resolver.go` §3.2). This is what makes "tenant licence never leaks
   into the player gate" a compile-path fact rather than a promise.
10. **`RequiredPurposes` fails closed for all four operation classes**
    (`operationClassPurposeMapping` contains four zero values, HDR-J-7
    unanswered), so `DeterminePlayerJurisdiction` cannot be wired for
    `play` by anyone, including this ADR. Casino's working integration
    uses `jurisdiction.Resolve`, not `DeterminePlayerJurisdiction`; §5.3
    mirrors casino.
11. **`jurisdictions.code` and an ISO-3166 country code are different code
    spaces** (ADR 0045 INC-6: `KM-ANJ` is sub-national; `MT`/`CO` match
    alpha-2 only coincidentally), and `jurisdictions.country_code` is
    nullable with zero rows populated by any migration. This is the
    decisive constraint on the operating-market rung — see §5.3.3.
12. **`risk_rules.game_id REFERENCES casino_games (id)`** (migration 0041
    line 53). A selection id structurally cannot be carried on a risk
    rule. Load-bearing for §6.2.1.

## 2. Context

Stage 9.2 lifts ADR 0047's two deferrals. Two capabilities are missing
from `PlaceBet` relative to `internal/casino.LaunchGame`/`postBet`:

- **No jurisdiction gate of any kind.** Casino resolves a jurisdiction on
  every launch and evaluates a per-title blocklist. Sportsbook resolves
  nothing and has no per-event/market/selection mechanism to evaluate.
- **No aggregate-risk capability.** Per-request risk (min/max amount,
  hard/soft limits, scoping) is fully wired and enforced on every bet.
  What is absent is (i) a rolling-window cap across a player's own bets
  and (ii) any notion of the book's aggregate potential payout on one
  outcome across all players.

Both are inert-today gaps, not live regressions — nothing is configured
anywhere on the platform that either control could enforce. This ADR
builds the mechanisms and leaves them unarmed.

## 3. Decision summary

- **Part C (§5):** wire `jurisdiction.Resolve` into `PlaceBet` and into
  the authenticated catalogue read paths, through **one shared evaluation
  function** used by both. Add **one new sportsbook-owned, platform-scoped
  table**, `sb_jurisdiction_restrictions`, carrying deny-only
  event/market/selection restrictions. **Add no column to any `sb_*`
  catalogue table**, so migration 0084/0085's write-authorization model is
  not reopened. Snapshot the resolved jurisdiction onto `sportsbook_bets`
  for historical stability. The licence/operating-market rung is specified
  in full but **NOT IMPLEMENTED — BLOCKED on HDR-J-7** for a reason
  sharper than ADR 0047's (§5.3.3).
- **Part B (§6):** add **one map entry** to `operationCumulativeSpecs` for
  the player-scoped rolling-window cap (no migration, no new threshold
  invented). Separately, add a **sportsbook-owned exposure gate** —
  `sb_exposure_limits` plus one advisory-lock-protected aggregate over
  `sportsbook_bets` — because cross-player, per-outcome exposure is
  structurally inexpressible in `risk_rules` and is a trading-book concept
  the ledger deliberately does not hold (doc 09 §1.7).
- **§7:** one authoritative composed call order, reconciled against
  `PlaceBet`'s actual current order and against ADR 0082's R1–R8.
- **§8:** exactly one new Human Decision Register item, **HDR-SB-1**.

## 4. What this ADR explicitly does NOT do

It does not redesign `internal/jurisdiction`, `internal/operatingmarket`,
`internal/risk`'s rule model, or `internal/ledger`. It answers no HDR-J
item. It seeds, invents or implies **no** country policy, no jurisdiction
restriction and no exposure threshold. It changes nothing about what is
posted to the ledger, about idempotency, or about ADR 0082's canonical
lock order (it extends the L0 class inventory; it inverts nothing). It
adds no casino code — casino parity follow-ups are recorded in §11, not
built here. It makes no commercial or legal determination.

---

# PART C — Sportsbook jurisdiction and market gating

## 5.1 The canonical flow, and which rungs this ADR delivers

```
  player (server-authenticated)
    └─ rung 1: player jurisdiction determination      → DELIVERED (§5.3.1)
        └─ rung 2: licence ceiling / operating-market
                   / tenant / brand / operation policy → SPECIFIED, NOT
                                                          IMPLEMENTED (§5.3.3)
            └─ rung 3: sportsbook catalogue restriction
                       (event / market / selection)    → DELIVERED (§5.2)
                └─ rung 4: bet placement enforcement    → DELIVERED (§7)
```

Rung ordering is licence-outward-in: the broadest permission (may we
operate here at all) is evaluated before the narrowest (may this outcome
be offered here). Rung 2 being unimplemented does **not** make rung 3
unsafe: rung 3 is a pure narrowing, and a narrowing applied without a
broader permission having been checked can only ever reject more, never
accept more (INV-SB-JUR-2, §10).

## 5.2 The data model — one new table, no new catalogue column

### 5.2.1 Why ADR 0047 §3's prescribed column is now the wrong answer

ADR 0047 §3 says to mirror `casino_games.jurisdiction_blocklist` with a
column on `sb_events`/`sb_selections`. That prescription predates
migration 0084. Five independent reasons it must not be followed:

1. **It would put a compliance control behind a provider-driven write
   path.** Migration 0084 scopes every `sb_*` INSERT/UPDATE to
   `app.platform_service_id = 'sportsbook_catalogue_sync'` — set only by
   `db.Pool.WithPlatformService`, used only by `SyncCatalogue`, which
   writes whatever the `Provider` adapter returns. A jurisdiction
   blocklist living on those rows would be writable *only* by the
   catalogue sync, i.e. a vendor feed would define the platform's
   jurisdiction blocking. That is architecturally inverted. Casino does
   not have this problem: `casino_games` is written by the *same*
   `app.platform_admin_principal_id` path that sets its blocklist.
2. **Every re-sync would be a chance to clobber operator compliance
   config.** `upsertEvent`/`upsertSelection` are `ON CONFLICT (external_ref)
   DO UPDATE SET ...`. A blocklist column would have to be excluded from
   every `SET` clause forever, by discipline, in a function whose entire
   job is to overwrite provider-derived fields. A separate table cannot be
   clobbered by a catalogue sync at all.
3. **Making the column admin-writable would reopen migration 0084's
   deliberate §3.3 decision** ("Deliberately NOT granted: platform-admin
   write on any `sb_*` table — there is no admin HTTP write path for these
   today, and pre-granting a capability with no handler would be an
   unaudited write surface"). The directive for this work forbids
   reopening that model.
4. **An array column cannot carry per-restriction authorization.** Each
   restriction is a compliance act that must name its
   `authorization_reference`, `reason_code` and actor — the discipline
   ADR 0045 §4 already imposes on every operating-market row. A
   `TEXT[]` column carries none of that; casino's own column has this
   weakness today (its provenance survives only in the admin endpoint's
   before/after audit record) and it should not be propagated.
5. **Three restriction levels would need three columns on three tables**
   versus one row shape in one table.

### 5.2.2 The table

New migration (expected number **0087**; 0086 is claimed by the parallel
Stage 9.2 casino-catalogue-dual-control workstream — the implementer takes
the next free number at implementation time and updates this ADR's text,
exactly as ADR 0081 §6 had to).

```sql
CREATE TABLE sb_jurisdiction_restrictions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope_kind              TEXT NOT NULL CHECK (scope_kind IN ('event','market','selection')),
    event_id                UUID REFERENCES sb_events (id),
    market_id               UUID REFERENCES sb_markets (id),
    selection_id            UUID REFERENCES sb_selections (id),
    jurisdiction_code       TEXT NOT NULL REFERENCES jurisdictions (code),
    -- Deny-only by construction (INV-SB-JUR-2). A single admitted value,
    -- not a free enum: widening this CHECK is an ADR amendment, not a
    -- migration someone can write on a Tuesday.
    restriction_kind        TEXT NOT NULL DEFAULT 'blocked' CHECK (restriction_kind = 'blocked'),
    status                  TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','withdrawn')),
    -- Required on every row (not only on an "enable", because every row
    -- here IS a restriction): ADR 0045 §4's authorization discipline.
    authorization_reference TEXT NOT NULL CHECK (btrim(authorization_reference) <> ''),
    reason_code             TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    created_by_actor_type   TEXT NOT NULL CHECK (created_by_actor_type = 'staff'),
    created_by_actor_id     UUID NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(event_id, market_id, selection_id) = 1),
    CHECK (
        (scope_kind = 'event'     AND event_id     IS NOT NULL) OR
        (scope_kind = 'market'    AND market_id    IS NOT NULL) OR
        (scope_kind = 'selection' AND selection_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX idx_sb_jur_restr_event_open
    ON sb_jurisdiction_restrictions (event_id, jurisdiction_code)
    WHERE status = 'active' AND event_id IS NOT NULL;
CREATE UNIQUE INDEX idx_sb_jur_restr_market_open
    ON sb_jurisdiction_restrictions (market_id, jurisdiction_code)
    WHERE status = 'active' AND market_id IS NOT NULL;
CREATE UNIQUE INDEX idx_sb_jur_restr_selection_open
    ON sb_jurisdiction_restrictions (selection_id, jurisdiction_code)
    WHERE status = 'active' AND selection_id IS NOT NULL;
```

**No `tenant_id` column, deliberately.** The catalogue this table
restricts is platform-wide, so a restriction on it is a platform-level
statement applying identically to every tenant. A *tenant's own* narrowing
belongs in `operating_country_policies` (the generic chain), which already
has tenant/brand/operation/product rungs and a trigger that forbids
widening past the licence ceiling. Putting a tenant dimension here would
create a second, ungoverned narrowing surface and would be the
"tenant-controlled override of platform-level jurisdiction" the directive
forbids.

### 5.2.3 RLS and trigger interaction — and why 0084/0085 stay closed

```sql
ALTER TABLE sb_jurisdiction_restrictions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_jurisdiction_restrictions FORCE ROW LEVEL SECURITY;

CREATE POLICY sb_jurisdiction_restrictions_read
    ON sb_jurisdiction_restrictions FOR SELECT USING (true);
```
Read-open is byte-identical to `casino_games_read` (migration 0084 §2) for
the identical data class: platform-uniform catalogue-availability policy
with no tenant dimension, therefore nothing tenant-confidential to leak
between tenants. This is *not* `operating_country_policies`' posture and
must not be confused with it — those rows disclose a specific tenant's
operating strategy (ADR 0045 §7.3) and are correctly narrow; these do not.

INSERT/UPDATE/DELETE-visibility policies are **byte-identical to
`casino_games_platform_admin_insert` / `_update` / `_delete_visibility`**
(migration 0084 §2): `app.platform_admin_principal_id` non-NULL,
`app.tenant_id` NULL, `app.player_account_id` NULL. Rationale is
unchanged from 0084: an authorized human platform admin, never a service
principal, never a tenant, never a player.

Triggers reuse the existing shared function, no new function:
```sql
CREATE TRIGGER sb_jurisdiction_restrictions_immutable_identity
    BEFORE UPDATE ON sb_jurisdiction_restrictions
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity(
        'id','scope_kind','event_id','market_id','selection_id',
        'jurisdiction_code','restriction_kind','created_at',
        'created_by_actor_type','created_by_actor_id');

CREATE TRIGGER sb_jurisdiction_restrictions_deny_delete
    BEFORE DELETE ON sb_jurisdiction_restrictions
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER sb_jurisdiction_restrictions_no_truncate
    BEFORE TRUNCATE ON sb_jurisdiction_restrictions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
```
`catalogue_enforce_immutable_identity` sets `NEW.updated_at := now()`,
which the column list above accommodates. The only mutable columns are
`status` and `reason_code`: a restriction is **withdrawn, never deleted**,
so the compliance history survives.

**Confirmation that migration 0084/0085 are not reopened:** this migration
adds no column, no policy, no trigger and no GUC to `casino_games`,
`sb_sports`, `sb_competitions`, `sb_events`, `sb_markets` or
`sb_selections`. It only adds foreign keys *pointing at* three of them,
and PostgreSQL bypasses RLS for referential-integrity checks (stated
explicitly in migration 0084's own header, citing ADR 0081 §2.3), so the
FKs create no policy interaction whatsoever. The `sb_*` write-authorization
model is untouched and stays closed.

### 5.2.4 Second migration change: the historical-stability snapshot

On the tenant-owned, RLS-protected `sportsbook_bets` table (migration
0078's own model — **not** the 0084 catalogue model), mirroring
`casino_launch_sessions.jurisdiction_code` (migration 0042) exactly:

```sql
ALTER TABLE sportsbook_bets
    ADD COLUMN jurisdiction_code TEXT REFERENCES jurisdictions (code);
```
Nullable; NULL means "the resolution did not resolve at placement time",
never "unknown" and never a default. `CREATE OR REPLACE FUNCTION
sportsbook_bets_enforce_immutable_fields()` gains
`OR NEW.jurisdiction_code IS DISTINCT FROM OLD.jurisdiction_code` to the
first `IF`, exactly as migration 0042 joined `casino_launch_sessions`'
own immutability trigger. This is the whole of the "as of what time"
mechanism (§5.5).

### 5.2.5 Third migration change (Part B): see §6.2.3

## 5.3 The three resolution rungs

### 5.3.1 Rung 1 — player jurisdiction (DELIVERED)

`PlaceBet` calls `jurisdiction.Resolve` with the **identical shape**
casino's `LaunchGame` uses, and then `AssertScope`:

```go
res, err := jurisdiction.Resolve(ctx, tx, jurisdiction.Params{
    TenantID:             params.TenantID,
    BrandID:              &params.BrandID,
    PlayerAccountID:      &params.PlayerAccountID,
    OperationClass:       jurisdiction.OperationPlay,
    RequestedByActorType: jurisdiction.ActorPlayer,
    RequestedByActorID:   &params.PlayerAccountID,
})
// then, before ever calling res.Code():
err = res.AssertScope(params.TenantID, &params.BrandID, &params.PlayerAccountID)
```

`OperationClass` is `OperationPlay`, not `OperationCatalogueAvailability`
— placement is a play decision; the catalogue read path (§5.4) uses
`OperationCatalogueAvailability`. Both are compile-time constants at their
call sites (canonical-model §4.4 Layer 1). No field is ever taken from a
request body: `PlaceBetParams` gains **no** jurisdiction field.

`jurisdiction.DeterminePlayerJurisdiction` is deliberately **not** called:
its `RequiredPurposes` owner fails closed for all four operation classes
pending HDR-J-7, it has no evidence adapter, and calling it would mean
choosing an evidence precedence, which is HDR-J-2/J-7's unanswered
question. When those are answered, the upgrade happens *inside*
`internal/jurisdiction`; sportsbook's call site does not change. That is
precisely why the call site is `Resolve`.

### 5.3.2 Rung 3 — sportsbook catalogue restriction (DELIVERED)

Mirrors `casino.evaluateJurisdictionBlocklist` semantics **exactly**,
including its two non-obvious rules:

- **K3-1 (arming):** no active restriction row for any of this bet's three
  catalogue levels ⇒ **the control is not armed for this bet and never
  denies**, whatever the resolution's outcome. Without this, every
  sportsbook bet on the platform would start denying with
  `jurisdiction_unresolved` the moment this code shipped, because every
  player-scoped resolution is `unresolved(no_signal)` today (§1.9). This
  is the single most important behavioural rule in Part C and it is not
  negotiable.
- **K3-2 (armed + unresolved):** armed and the resolution did not resolve
  ⇒ **deny with `jurisdiction_unresolved`, never `jurisdiction_blocked`**.
  These stay distinguishable internally and in the audit record; the HTTP
  boundary collapses them into one player-facing message (K3-6).

Honest consequence, which must appear on the admin write surface: because
no player-scoped resolution resolves today, **arming any restriction row
blocks that event/market/selection for every player until player
jurisdiction resolution exists.** This is identical to casino's behaviour
and is the fail-closed direction.

### 5.3.3 Rung 2 — licence ceiling / operating-market policy (SPECIFIED, NOT IMPLEMENTED — BLOCKED on HDR-J-7)

This is a **sharper reason than ADR 0047 gave**, and it is not "there is
no country policy configured yet".

`operatingmarket.ResolveOperatingCountryPolicy` requires
`Query.CountryCode`, "server-resolved, ISO-3166-1 alpha-2". There are
exactly two conceivable sources, and both are closed today:

- **`jurisdictions.country_code`** — ADR 0045 INC-6 fences it as
  "administrative metadata, never a country→jurisdiction resolver";
  `jurisdictions.code` is a *different code space* (`KM-ANJ` is
  sub-national). It is nullable and **no migration populates a single
  row**. Even setting the fence aside, there is nothing to read.
- **A player residence/location evidence value** (`identity`'s declared
  residence, `kyc`'s verified residence, a location signal) — choosing
  which of these is authoritative for a market-access decision *is*
  HDR-J-2/HDR-J-7, whose single canonical owner
  (`jurisdiction.RequiredPurposes`) fails closed by design. Reading one
  directly would be engineering answering a legal question unilaterally.

**Therefore rung 2 cannot be wired by anyone in Stage 9.2, and no stub is
shipped** (a stub that always returns "not determinable" is dead code with
a fail-open shape; CLAUDE.md's no-fake-completion rule is better served by
its absence plus this record). What is delivered instead is the exact
contract the later wave implements verbatim:

```go
// evaluateOperatingMarket consults the licence-ceiling / tenant / brand /
// operation-policy chain for a player whose OPERATING COUNTRY has been
// determined by the single canonical owner of that determination.
//
// BLOCKED on HDR-J-7. Not implemented in Stage 9.2 - see ADR 0083 §5.3.3.
// When implemented:
//   - countryCode MUST come from the canonical player-jurisdiction
//     determination path, never from jurisdictions.country_code (ADR 0045
//     INC-6 / INV-M-4) and never from a client.
//   - `armed` is false, and permitted is true, ONLY when no operating
//     country is determinable at all. It is NEVER false because a policy
//     is missing: operatingmarket.OutcomeNotConfigured is a DENY
//     (ADR 0045 INV-M-2 - the tenant-scope enabled row is a mandatory
//     opt-in), as is every outcome other than OutcomePermitted.
//   - AsOf is the caller's own placement timestamp, passed explicitly;
//     ResolveOperatingCountryPolicy never calls time.Now() (INV-M-3).
//   - OperationCode is the compile-time constant "wagering" and
//     ProductCode is the compile-time constant "sportsbook" - both are
//     already seeded vocabulary rows (migrations 0076 / 0045). Neither is
//     ever a wire string.
//   - Result.AssertScope(tenantID, &brandID) is called and refused on.
//   - The eleven-valued Outcome is NEVER collapsed into the player-facing
//     response; the player sees one opaque unavailability reason, and the
//     blocking rung is reachable only through the staff-only
//     ExplainOperatingCountryPolicy (ADR 0045 §4 property 2).
func evaluateOperatingMarket(
    ctx context.Context, tx pgx.Tx,
    tenantID uuid.UUID, brandID *uuid.UUID,
    countryCode string, asOf time.Time,
) (permitted bool, outcome operatingmarket.Outcome, err error)
```

**An arming switch for this rung is forbidden**, and this is a ruling, not
a preference: a tenant- or operator-settable "skip the operating-market
check" flag would be a tenant-controlled override of a platform-level
jurisdiction decision and would let a tenant widen past its own licence
ceiling — both explicit non-negotiables. The only legitimate reason this
rung does not run is that no operating country is determinable at all.

**Consequence to record in `docs/governance/task-registry.md`
(`SB-JUR-RUNG2-1`):** on the day an operating-country determination
becomes available, every tenant serving sportsbook must already have its
`licence_country_ceilings` and tenant-scope `operating_country_policies`
rows configured, or its bets will correctly begin failing closed. That
sequencing is a launch dependency, not a defect, and it must be scheduled
alongside the HDR-J-7 answer rather than discovered by it.

## 5.4 The two enforcement points, and the one shared implementation

Both enforcement points call **the same function**. There are not two
implementations of "may this player bet on this selection".

```go
// internal/sportsbook/jurisdiction.go (new file)

// AvailabilityContext is the server-derived context a jurisdiction
// evaluation runs in. Every field is resolved from the authenticated
// session, never from a request body, header, query or path segment.
// The ZERO VALUE means ANONYMOUS: no player exists to resolve a
// jurisdiction for, so no annotation is applied and the catalogue is
// returned exactly as it is today (§5.4.1).
type AvailabilityContext struct {
    TenantID        uuid.UUID
    BrandID         uuid.UUID
    PlayerAccountID uuid.UUID
}

func (a AvailabilityContext) IsAnonymous() bool { return a.PlayerAccountID == uuid.Nil }

// catalogueScope is the three catalogue ids one selection resolves into.
// PlaceBet already has all three from getSelectionWithContext - no extra
// query is introduced on the bet path to obtain them.
type catalogueScope struct{ EventID, MarketID, SelectionID uuid.UUID }

// AvailabilityDecision is the single shared gate's output. A restriction
// is reported as a RESULT, never a Go error - identical convention to
// PlaceBetResult/LaunchGameResult, for the identical reason (the audit
// record for a denial must commit in the same transaction).
type AvailabilityDecision struct {
    Available  bool
    DenialCode string // DenialCodeJurisdictionBlocked | DenialCodeJurisdictionUnresolved
}

const (
    DenialCodeJurisdictionUnresolved = "jurisdiction_unresolved"
    DenialCodeJurisdictionBlocked    = "jurisdiction_blocked"
)

// ResolvePlayerJurisdiction is the ONE call both enforcement points make.
// opClass is jurisdiction.OperationPlay for placement and
// jurisdiction.OperationCatalogueAvailability for a catalogue read - a
// compile-time constant at each call site, never a parameter derived from
// input. It calls jurisdiction.Resolve and then Resolution.AssertScope,
// and returns the Resolution unchanged; it never inspects Code() itself.
func ResolvePlayerJurisdiction(
    ctx context.Context, tx pgx.Tx,
    ac AvailabilityContext, opClass jurisdiction.OperationClass,
) (jurisdiction.Resolution, error)

// loadActiveRestrictions returns the jurisdiction codes actively
// restricted at ANY of scope's three levels, in ONE indexed query.
func loadActiveRestrictions(ctx context.Context, tx pgx.Tx, scope catalogueScope) ([]string, error)

// evaluateJurisdictionRestriction is the byte-for-byte behavioural mirror
// of casino.evaluateJurisdictionBlocklist: K3-1 (empty => not armed =>
// never denies) and K3-2 (armed + non-Resolved => Unresolved, never
// Blocked). Pure function of its two arguments, unit-testable without a
// database, exactly as casino's is.
func evaluateJurisdictionRestriction(restricted []string, res jurisdiction.Resolution) (AvailabilityDecision, error)
```

### 5.4.1 Enforcement point 1 — catalogue visibility: MARK UNAVAILABLE, never omit

**Decision: a jurisdiction-restricted event/market/selection is RETURNED
and MARKED unavailable. It is never silently omitted.** Justification,
weighed against the casino precedent as the directive requires:

- **Casino's own precedent is weaker than "hide" and weaker than "mark".**
  `casino.ListAvailableGames` filters on `status = 'active'` and the
  tenant/brand availability join only — it applies **no** jurisdiction
  filtering at all, and returns `jurisdiction_blocklist` as a field on the
  game. Casino's only jurisdiction enforcement is at launch time. So
  "mark unavailable" is strictly *stronger* than the existing precedent
  and does not contradict it.
- **The anonymous-read constraint decides the rest.** `GET /v1/sportsbook/sports`
  and `GET /v1/sportsbook/events/{id}` have genuinely anonymous readers
  (ADR 0081 §2.3). Omitting rows would make the response shape differ
  between anonymous and authenticated callers for reasons the client
  cannot see, and would break deep-link stability for a player who
  bookmarked an event. Marking keeps one shape.
- **Hiding is not a security control here.** The authoritative check is
  enforcement point 2. Treating catalogue omission as the control is
  exactly the "authorization inferred from the UI" pattern CLAUDE.md
  forbids.
- **The marker must not leak the blocking rung.** One opaque player-facing
  reason only (ADR 0045 §4 property 2 and casino's K3-6, applied here).

Surface:
```go
// New fields on EventSummary, MarketDetail and Selection (internal/
// sportsbook/types.go). Default true - an anonymous read and an
// unannotated read both return exactly today's behaviour.
Available         bool   `json:"available"`
UnavailableReason string `json:"unavailable_reason,omitempty"`

// Annotators. Both no-op for an anonymous AvailabilityContext.
func AnnotateCatalogueAvailability(ctx context.Context, tx pgx.Tx, cat []SportCatalogue, ac AvailabilityContext) error
func AnnotateEventAvailability(ctx context.Context, tx pgx.Tx, d *EventDetail, ac AvailabilityContext) error
```
Cost: for an authenticated read, one `jurisdiction.Resolve` (a
no-database-round-trip call for a player-scoped subject today) plus one
`SELECT ... FROM sb_jurisdiction_restrictions WHERE status = 'active'`
covering the whole browse tree, evaluated in memory. Zero rows today.

Restrictions flow **downward** in the catalogue tree: a restriction on an
event marks every market and selection beneath it unavailable. It never
flows upward (a restricted selection does not mark its market
unavailable) — a market with one restricted selection is still a real,
bettable market.

### 5.4.2 Enforcement point 2 — bet placement, independently re-checked

`PlaceBet` **re-runs the entire gate itself**, against the specific
`selection_id` the client supplied, resolving that selection's own event
and market server-side from `getSelectionWithContext`. It never trusts
that the client went through the catalogue, never accepts a
client-supplied event/market id, and never consults a cached availability
answer. A client that skips the UI and POSTs a restricted `selection_id`
directly is rejected by the same code path with the same decision.

## 5.5 Historical / event-time semantics — unchanged, and mirrored from casino

The existing platform mechanism is **snapshot the decision, do not make
the policy bitemporal**: `casino.LaunchGame` persists the resolved
`jurisdiction_code` onto `casino_launch_sessions` (migration 0042,
immutable by that migration's own trigger) and every later bet in the
round reuses that snapshot rather than re-resolving.

Sportsbook mirrors it exactly: the resolved code (or NULL) is written to
`sportsbook_bets.jurisdiction_code` at placement and frozen by
`sportsbook_bets_enforce_immutable_fields` (§5.2.4). A later change to
`sb_jurisdiction_restrictions`, to operating-market policy, or to the
resolver itself **cannot** alter what a placed bet's jurisdiction was
determined to be. `jurisdiction.PurposeHistoricalReporting` remains
uncomputable by construction (`ErrHistoricalPurposeNotComputable`), which
is what forces every historical consumer onto this snapshot. Nothing about
these semantics changes.

`sb_jurisdiction_restrictions` is therefore deliberately **not**
effective-dated (unlike `operating_country_policies`): the decision
snapshot provides historical stability, and `status = 'withdrawn'` plus
the deny-delete trigger provide the configuration history. Adding
effective dating would duplicate the snapshot's job at real complexity
cost.

## 5.6 Closing ADR 0047 §5's `FOR SHARE` item

`getSelectionWithContext` reads `sb_selections`/`sb_markets`/`sb_events`
unlocked. ADR 0047 §5 deferred this as "inert today, required before any
live-odds/real-provider feed". It stays unlocked, and this ADR **closes
the item as resolved-by-other-means**: migration 0084 now forbids any
write to those tables except under
`app.platform_service_id = 'sportsbook_catalogue_sync'`, so the set of
possible concurrent writers is exactly one known startup process, and
migration 0084 §7 keeps `odds_*` deliberately mutable while migration
0082 §1.1 freezes the accepted bet's own odds. The odds-changed rejection
in `PlaceBet` remains the correct control. If a live in-play odds feed is
ever built, it must revisit this and the exposure query's own snapshot
semantics (§6.2.4) together.

---

# PART B — Cumulative risk and exposure

## 6.0 Two different concepts, deliberately separated

| | B1 — player cumulative stake | B2 — book exposure |
| --- | --- | --- |
| Question | "has this player staked more than X in the last hour?" | "if this selection wins, how much would the book owe, across all open bets?" |
| Aggregation axis | one player | all players in one tenant |
| Granularity | tenant/brand/player/asset | event / market / selection, per asset |
| Measured from | `ledger_entries` (authoritative) | `sportsbook_bets.potential_return` (domain projection) |
| Home | `internal/risk` (`risk_rules`) | `internal/sportsbook` |
| doc 09 name | — | **Exposure** (§1.7), explicitly *not* Liability |

Conflating them is how a platform grows a second financial-truth system
(doc 09 §1.7's "load-bearing rule"). They are built separately.

## 6.1 B1 — player-scoped cumulative stake limit

### 6.1.1 The entire change

One map literal in `internal/risk/cumulative.go`. No migration. No
interface change. No new exported symbol.

```go
OperationSportsbookBet: {
    TransactionTypes:     []string{"sportsbook_bet"},
    ReversalTypes:        nil, // see §6.1.2 - sportsbook_void does not exist
    MeasuredAccountTypes: []string{"player_cash"},
    IgnoredAccountTypes:  []string{"player_locked_cash"},
    ConsumingDirection:   directionDebit,
},
```

Verification of each field against HEAD:
- `TransactionTypes`: `ledger.TxSportsbookBet` == `"sportsbook_bet"`, the
  seventeenth admitted `transaction_type` (migration 0078 §1). Correct.
- `MeasuredAccountTypes`: `PlaceBet` debits `player_cash`. The stake is
  the usage. Correct.
- `IgnoredAccountTypes`: `PlaceBet` credits `player_locked_cash` — a
  second *player-owned* leg with the same `player_account_id` denormalised
  onto it. Without this declaration the two legs cancel and usage computes
  as exactly zero: the fail-open ADR 0031 §32(a) existed to close, proven
  by `TestEvaluate_CumulativeUsageIsLegAwareForATwoPlayerOwnedLegOperation`
  against the structurally identical `Dr player_cash / Cr player_withdrawal_hold`
  shape. `player_locked_bonus` is **deliberately NOT declared**:
  bonus-funded sportsbook stakes are blocked platform-wide (ADR 0038 §9;
  `ledger.assertNoBonusSetEntries`/HR-9). If that is ever unblocked,
  `ErrUnrecognizedCumulativeLeg` stops the evaluation before it can
  under-count — which is the correct order of events.
- `ConsumingDirection`: `directionDebit`. A stake consumes capacity.

### 6.1.2 `ReversalTypes` must be empty — correcting ADR 0047 §4

`sportsbook_void` is not an admitted `ledger_transactions.transaction_type`
value and no void/settlement posting path exists (ADR 0082 §5.3:
"NOT IMPLEMENTED — no `ledger.Post` call site exists"). Declaring a
non-existent type would be inert at query time (`= ANY` matches nothing)
but would be a false claim in the one place a future author will read for
truth.

Empty `ReversalTypes` is **safe and conservative**: a voided bet continues
to consume the player's rolling-window capacity until the reversal type
exists and is declared. That over-counts, never under-counts — the same
explicitly-reasoned position `OperationBonusConversion` already takes
(`cumulative.go`: "it never causes an UNDER-count, which is the direction
that would actually be unsafe here").

**Binding forward requirement (INV-SB-CUM-1, §10):** whoever implements
sportsbook settlement/void must add the reversal transaction type(s) to
this spec **in the same change** as the migration that widens the
`transaction_type` CHECK. A widening migration without the corresponding
spec update is a defect `code-reviewer` must reject.

### 6.1.3 No new business threshold is invented

A cumulative sportsbook cap is configured exactly like every other risk
rule: a `risk_rules` row with `operation = 'sportsbook_bet'`,
`limit_kind = 'cumulative_amount'`, a rolling `time_window`, a `threshold`
and its denomination (`asset_code`, or `threshold_exponent` for an
asset-agnostic rule), authored by a `tenant_admin`/`risk_manager` through
the existing, audited `POST /v1/admin/risk/rules`. **This ADR creates no
row, seeds nothing, and recommends no number.** Because this is ordinary
per-tenant commercial configuration of an already-shipped, already-audited
control surface — identical in kind to a max-stake limit — it needs **no
Human Decision Register entry.** (§6.2 does; see HDR-SB-1.)

### 6.1.4 The unconfigured behaviour, stated precisely

Verified by reading `risk.Evaluate`, not assumed:

- **Unconfigured ⇒ no check applied ⇒ ALLOW.** `listEffectiveRules`
  returns the rules configured for the operation; none matching yields
  `RiskDecision{Outcome: OutcomeAllow, Code: CodeAllowed}`. This is
  genuinely **fail-OPEN-when-unconfigured**, it is the platform's
  deliberate and uniform posture for every tenant-configurable risk rule,
  and it is stated plainly here rather than described as fail-closed.
- **Misconfigured or unmeasurable ⇒ fail CLOSED.** Every one of
  `ErrUnsupportedCumulativeOperation`, `ErrInvalidCumulativeSpec`,
  `ErrUnrecognizedCumulativeLeg`, `ErrConflictingRules`,
  `ErrMissingAmount/Asset/Player/Jurisdiction/LicensingMode/ScopeContext`,
  `ErrTenantScopeMismatch`, `ErrPlayerScopedConnection` is a non-nil error
  that `PlaceBet` must propagate, aborting the whole transaction. It is
  never a decline and never an ALLOW. `sportsbook.evaluateAndAuditRisk`
  and `classifyRiskOutcome` already implement this correctly at HEAD and
  need no change.
- Adding this entry **closes ADR 0047 §5's cumulative-rule write-time
  validation gap for `sportsbook_bet` specifically**: a
  `cumulative_amount` rule for that operation becomes evaluable instead of
  erroring on every subsequent bet. The general gap (the same lever exists
  for `casino_launch`, `deposit`, `withdrawal`, `bonus_grant`) remains
  open and is out of scope here — it belongs in `risk.CreateRule`.

### 6.1.5 One composition win from Part C

`PlaceBet`'s `RiskRequest` currently leaves `JurisdictionCode` empty, so a
single jurisdiction-scoped rule authored for `sportsbook_bet` would make
every sportsbook bet fail closed with `ErrMissingJurisdiction` — a
self-inflicted outage lever ADR 0047 §1 row E noted as "safe today but a
smaller instance of the same wiring gap". Part C removes it: §7 step 9
populates `JurisdictionCode` from the same resolution the jurisdiction
gate used, exactly as `casino.LaunchGame` does. **The two workstreams must
land together, or this lever stays armed.**

## 6.2 B2 — cross-player book exposure

### 6.2.1 Why this cannot live in `risk_rules` (answering doc 09 Open Question 3)

Doc 09 §3.4 left open "whether a per-market or per-selection exposure
ceiling [should be] a first-class `risk_rules` scope dimension". **Answer:
no.** Four structural reasons, each verified:

1. **No scope axis exists.** `risk_rules`' dimensions are tenant, brand,
   jurisdiction, licensing_mode, player, product, operation, provider,
   game, asset, payment_method. The only entity-level axis is
   `game_id UUID REFERENCES casino_games (id)` (migration 0041 line 53) —
   a selection id cannot be stored there, and repurposing it would be a
   cross-domain FK violation.
2. **Aggregation is player-scoped by construction.**
   `cumulativeUsage` filters `le.player_account_id = $2`, and
   `LimitCumulativeAmount` returns `ErrMissingPlayer` without one. Exposure
   is explicitly the aggregate *across* players.
3. **The measured quantity is not in the ledger.** `cumulativeUsage` reads
   `ledger_entries`. The ledger holds the **stake** (in
   `player_locked_cash`) — the platform's Liability. It deliberately does
   not hold `potential_return`, which is "a DOMAIN PROJECTION (ADR 0038
   §2), never a ledger-visible fact". Teaching `internal/risk` to read
   `sportsbook_bets` would couple the generic risk engine to one product's
   schema.
4. **doc 09 §1.7 already ruled on ownership:** Exposure is "computed from
   the same canonical Bet/BetLeg data the rest of the domain model already
   holds", is "read-only trading intelligence", and must never become a
   second source of truth for what the platform owes.

So exposure is a sportsbook-domain control that consumes the same
canonical data, sits alongside `risk.Evaluate` (never instead of it), and
adds no new `risk` `Operation` or `LimitKind`.

### 6.2.2 The exact measure

For tenant `T`, optional brand `B`, asset `A`, and catalogue scope `S`
(one of an event, a market, or a selection):

```
Exposure(T, B, A, S) = SUM(sportsbook_bets.potential_return)
  WHERE tenant_id = T
    AND (B IS NULL OR brand_id = B)
    AND asset_code = A
    AND status = 'open'
    AND the bet's selection resolves into S
```

- **Gross potential payout, not net.** `potential_return` is
  `stake × decimal_odds` and already includes the returned stake. doc 09
  §1.7 defines Exposure as "the aggregate potential payout the book would
  owe". Gross is also the conservative (larger) figure and is a plain
  `SUM` of a stored column with no arithmetic. The net figure
  (`potential_return − stake`) is trivially derivable later; the admin
  surface must state on the record which measure the configured number is
  denominated in, so a human setting it is not guessing.
- **Per asset, never netted across assets** — doc 09's own rule.
- **`status = 'open'` only.** Settled and void bets carry no forward
  liability. Today `'open'` is the only status ever written, so this
  predicate is future-proofing, not filtering.

### 6.2.3 The configuration table (third migration change)

```sql
CREATE TABLE sb_exposure_limits (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL REFERENCES tenants (id),
    -- NULL = every brand in this tenant. Mirrors casino_game_availability's
    -- own (brand_id = $ OR brand_id IS NULL) precedence precedent.
    brand_id                  UUID,
    scope_kind                TEXT NOT NULL CHECK (scope_kind IN ('event','market','selection')),
    asset_code                TEXT NOT NULL REFERENCES assets (code),
    -- NUMERIC(38,0), matching risk_rules.threshold - never BIGINT, never
    -- float (CLAUDE.md: an 18-exponent asset's aggregate exceeds int64).
    -- Compared in Go via *big.Int, reusing risk's numericToBigInt
    -- discipline (a negative NUMERIC exponent is refused, a positive one
    -- is scaled, never truncated).
    max_open_potential_payout NUMERIC(38,0) NOT NULL CHECK (max_open_potential_payout > 0),
    status                    TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    authorization_reference   TEXT NOT NULL CHECK (btrim(authorization_reference) <> ''),
    reason_code               TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    created_by_actor_type     TEXT NOT NULL CHECK (created_by_actor_type = 'staff'),
    created_by_actor_id       UUID NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE UNIQUE INDEX idx_sb_exposure_limits_brand_key
    ON sb_exposure_limits (tenant_id, brand_id, scope_kind, asset_code)
    WHERE status = 'active' AND brand_id IS NOT NULL;
CREATE UNIQUE INDEX idx_sb_exposure_limits_tenant_key
    ON sb_exposure_limits (tenant_id, scope_kind, asset_code)
    WHERE status = 'active' AND brand_id IS NULL;

ALTER TABLE sb_exposure_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_exposure_limits FORCE ROW LEVEL SECURITY;
```

RLS: the **two-policy shape of `sportsbook_bets` (migration 0078)** —
`tenant_staff_scope` FOR ALL under tenant-only scope
(`app.tenant_id` matches, `app.player_account_id` NULL), and **no player
policy at all**: a limit is trading-book intelligence and has no
player-facing read path, mirroring `risk_rules`' own posture. Immutability
trigger freezing `id`, `tenant_id`, `brand_id`, `scope_kind`,
`asset_code`, `created_at`, `created_by_actor_*`; a limit is **disabled
and superseded, never edited in place** (`max_open_potential_payout` is
therefore also immutable — changing a ceiling means a new row). Deny-delete
and no-truncate triggers as elsewhere.

**Per-scope-kind, not per-instance, deliberately.** One row says "for this
tenant/brand, no single *selection* may carry more than X open potential
payout in asset A". Per-event configuration would be unmaintainable
against a live feed that mints thousands of events, and nothing in the
Blueprint or any ADR asks for per-instance ceilings. This is the smallest
shape that is actually operable.

Supporting index on the aggregate's source table:
```sql
CREATE INDEX idx_sportsbook_bets_open_exposure
    ON sportsbook_bets (tenant_id, asset_code, selection_id)
    WHERE status = 'open';
```
Partial, so it stays proportional to the *open* book rather than to all
history. It serves the selection-level lookup directly, and the
market/event-level lookups as a tenant+asset scan hash-joined to the small
selection set produced by `idx_sb_selections_market`. The implementer must
`EXPLAIN` all three shapes and record the plans.

**Cost when unarmed — this is the "no new expensive scan" answer.** The
only unconditional added work on the bet path is one indexed `SELECT` on
`sb_exposure_limits` (`tenant_id`, `asset_code`). With zero rows
configured — the shipped state — no aggregate query runs, no advisory
lock is taken, and `sportsbook_bets` is not touched at all.

### 6.2.4 The evaluation function

```go
// internal/sportsbook/exposure.go (new file)

// exposureParams is evaluateExposureLimits' input. Every id is
// server-derived; IncrementalPotentialReturn is THIS bet's own
// potential_return, computed by computePotentialReturn before this call.
type exposureParams struct {
    TenantID    uuid.UUID
    BrandID     uuid.UUID
    Scope       catalogueScope // Event/Market/Selection, from getSelectionWithContext
    AssetCode   string
    IncrementalPotentialReturn int64
}

// ExposureDecision is the gate's output. Deliberately carries NO amount
// and NO threshold: the aggregate and the ceiling are trading-book
// intelligence and must never reach a player-facing response. ScopeKind
// and LimitID exist for the audit record only.
type ExposureDecision struct {
    Breached  bool
    ScopeKind string
    LimitID   uuid.UUID
}

// evaluateExposureLimits is a PRE-POSTING DECISION GATE. It writes
// nothing, posts nothing, takes no wallet_balance_projection lock, and
// never reads a balance. Any non-nil error is fail-closed: the caller
// propagates it and the whole transaction aborts.
//
// tx MUST be tenant-scoped and MUST NOT be player-scoped - the aggregate
// spans every player in the tenant, which sportsbook_bets' own
// tenant_staff_scope policy permits and its player policy does not. This
// is asserted, exactly as risk.Evaluate's verifyConnectionScope asserts
// the same precondition, rather than assumed.
func evaluateExposureLimits(ctx context.Context, tx pgx.Tx, p exposureParams) (ExposureDecision, error)
```

Algorithm, in order:
1. `SELECT` active limits for `(tenant_id, asset_code)` where
   `brand_id = p.BrandID OR brand_id IS NULL`, brand-specific winning over
   tenant-wide per `scope_kind` (the `casino_game_availability`
   `ORDER BY (brand_id IS NULL) ASC` precedent). **None ⇒ not armed ⇒
   return `{Breached: false}` immediately, taking no lock.**
2. Acquire the **class L0.6** advisory lock (§7.3), keyed on the
   **event id** — one lock, whatever mix of scope kinds is configured.
3. For each configured `scope_kind`, compute the aggregate (§6.2.2), add
   `IncrementalPotentialReturn`, compare against
   `max_open_potential_payout` as `*big.Int`. First breach wins; evaluate
   broadest first (event → market → selection) so the reported
   `ScopeKind` is the broadest true statement.
4. Return.

### 6.2.5 Why one event-scoped advisory lock is the right lock

Exposure aggregates at three levels, but **any two bets whose aggregates
can interact at market or selection level necessarily share the same
event** (selection ⊂ market ⊂ event). One lock keyed on the event id
therefore closes every race at all three levels, and — critically —
removes the need to invent a within-class ordering rule for multiple L0.6
locks, which is exactly the class of rule ADR 0082 R2 exists to avoid
having to invent.

Correctness under READ COMMITTED: `pg_advisory_xact_lock` is held until
the holder's transaction commits or rolls back, and the holder's
`sportsbook_bets` row becomes visible at that same commit. The second
transaction acquires the lock only afterwards, and — being READ COMMITTED
— takes a fresh snapshot for its next statement, so it sees the first
bet's row. This is the identical argument `internal/risk`'s own cumulative
advisory lock relies on, already proven by
`internal/risk/cumulative_race_integration_test.go`.

Accepted consequence: concurrent bets on the same event serialize at this
lock **when a limit is configured**. Zero impact when unarmed. If a real
provider or in-house engine ever makes same-event concurrency a
throughput problem, the refinement is finer-grained keys plus an explicit
within-class ordering rule — a future amendment to this ADR and to ADR
0082 §2.1, not a silent change.

### 6.2.6 Tenant isolation is structural, not a policy choice

`sportsbook_bets` carries `ENABLE`+`FORCE` RLS. `PlaceBet`'s transaction
is tenant-scoped, so the aggregate **structurally cannot** see another
tenant's bets. A platform-wide, cross-tenant exposure figure is therefore
not merely unimplemented — it is unreachable without breaking tenant
isolation, and it is forbidden (INV-SB-EXP-1, §10). This is also the
commercially correct semantics under the hybrid licensing model (ADR
0006): a bring-your-own-licence tenant carries its own book.

### 6.2.7 Provider neutrality

Nothing in §6.2 references the mock provider, a vendor protocol, or a
vendor field name. The aggregate is computed from `sportsbook_bets` —
platform-owned rows the platform writes regardless of which `Provider`
adapter produced the catalogue — and the limits are platform
configuration. A real provider adapter, or a future in-house trading
engine, uses this mechanism unchanged; an in-house engine's own trading
logic would *additionally* consume the same aggregate as an input to
market suspension (doc 09 §3.6), which this design does not preclude and
does not build.

---

## 7. The composed call order inside `PlaceBet` — the authoritative statement

### 7.1 Reconciled against the actual current order

`PlaceBet` at HEAD does: validate → idempotency short-circuit →
`getSelectionWithContext` → status/odds checks → RG → `resolveLicensingMode`
→ Risk → `GetOrCreateAccounts` → build `betInput` →
`LockProjectionsForPosting` → balance check → `computePotentialReturn` →
`ledger.Post` → `insertBet` → cross-check → audit.

New order, with every step labelled **[unchanged]**, **[NEW]** or
**[MOVED]**:

1. **[unchanged]** Structural input validation.
2. **[unchanged]** Idempotency short-circuit
   (`findBetByIdempotencyKey`). **Stays strictly first among all
   evaluation.** A retry must return the original bet without
   re-evaluating jurisdiction, RG, risk or exposure — re-evaluating could
   turn a previously accepted bet into a rejection and would take locks a
   no-op does not need.
3. **[unchanged]** `getSelectionWithContext` — unlocked read; yields
   `SelectionID`, `MarketID`, `EventID` for free. **No new query is
   introduced on the bet path to build `catalogueScope`.**
4. **[unchanged]** Event/market/selection status checks and the
   odds-changed check.
5. **[NEW]** `jurisdiction.Resolve(..., OperationPlay, ...)` →
   `Resolution.AssertScope(...)`. Takes **no lock** (player-scoped
   `Resolve` does not touch the database).
6. **[NEW]** `loadActiveRestrictions(scope)` → unlocked `SELECT`.
7. **[NEW]** `evaluateJurisdictionRestriction(...)` → on denial, write the
   `sportsbook_bet.denied_by_jurisdiction_policy` audit record and return
   `PlaceBetResult{Accepted:false, RejectionCategory: RejectionJurisdictionDenied,
   RejectionCode: jurisdiction_blocked|jurisdiction_unresolved}`.
   Still **no lock**.
   *(Rung 2, `evaluateOperatingMarket`, occupies this position when HDR-J-7
   is answered — §5.3.3. It too takes no lock.)*
8. **[unchanged]** RG: `evaluateAndAuditEligibility` → `rg.EvaluateEligibility`
   → **L0.4** (`rg.lockPerson`).
9. **[unchanged call, CHANGED field]** `resolveLicensingMode`, then
   `evaluateAndAuditRisk` with `RiskRequest.JurisdictionCode` **now
   populated** from step 5's resolution when `Outcome() == Resolved`
   (empty otherwise, never defaulted — casino's identical pattern).
   May take **L0.5** (risk cumulative advisory), now reachable for
   sportsbook because §6.1 adds the spec.
10. **[MOVED]** `computePotentialReturn` — moved from after the balance
    check to here, because step 11 needs it. Takes no lock
    (`assetregistry.GetAsset` is an unlocked read plus `*big.Rat`
    arithmetic). **Stated behaviour change:** an asset-registry or
    rounding error now surfaces before the insufficient-funds decline
    rather than after it. Both are fail-closed; the new order is strictly
    better (a structurally uncomputable bet errors before any projection
    row is locked or materialised).
11. **[NEW]** `evaluateExposureLimits(...)` → **L0.6** (event-scoped
    advisory) **only if armed**. On breach: audit record, then
    `PlaceBetResult{Accepted:false, RejectionCategory: RejectionExposureLimit}`.
12. **[unchanged]** `ledger.GetOrCreateAccounts` (ADR 0082 §3.2 canonical
    creation order).
13. **[unchanged]** Build `betInput` — now also carrying nothing new; the
    jurisdiction snapshot goes on the `sportsbook_bets` row, not the
    ledger transaction.
14. **[unchanged]** `ledger.LockProjectionsForPosting(ctx, tx, betInput)`
    — **L3**, the single pre-lock over every projection row this posting
    touches (R1/R3/R4/R6).
15. **[unchanged]** Read the balance from `locked.Balance(cashAccountID)`;
    insufficient funds ⇒ audit + decline.
16. **[unchanged]** `ledger.Post(ctx, tx, betInput)` — the **same**
    `betInput` (R3) — **L4**.
17. **[unchanged, one new field]** `insertBet`, now also writing
    `jurisdiction_code` (§5.2.4). **Exception E-3** (§7.3).
18. **[unchanged]** Ledger-transaction cross-check, then the
    `sportsbook_bet.placed` audit record, now carrying `jurisdiction_code`
    and `jurisdiction_outcome` in its metadata.

### 7.2 ADR 0082 compliance check

Resulting lock acquisition sequence:

```
(no lock)  steps 1-7   — jurisdiction gate
L0.4       step 8      — RG person advisory
L0.5       step 9      — risk cumulative advisory (when scoped)
L0.6       step 11     — sportsbook event-exposure advisory (when armed)   [NEW]
L3         step 14     — wallet_balance_projection, ascending account id
L4         step 16     — ledger_transactions key insert + entries + trigger
L1         step 17     — sportsbook_bets unique-index wait        [E-3, pre-existing]
```

- **R8 (advisory locks strictly precede row locks): SATISFIED.** Every
  L0 lock, including the new L0.6, is taken before L3. The jurisdiction
  gate takes no lock at all, so inserting it at step 5 cannot violate R8
  by construction.
- **R1 / R3 (one pre-lock step, no partial pre-locking): UNTOUCHED.**
  Neither new gate locks or reads a `wallet_balance_projection` row, and
  the `betInput` fed to the pre-lock is still byte-identical to the one
  fed to `Post`.
- **R4 (only `internal/ledger` locks the projection): UNTOUCHED.** No new
  `FOR UPDATE` on that table appears anywhere;
  `TestLockOrder_NoProjectionForUpdateOutsideLedgerPackage` continues to
  pass unmodified.
- **R5 / R6 / R7: UNTOUCHED.** Nothing about `Post`'s internals, entry
  order, or projection materialisation changes.
- **A rejection at step 7 or step 11 leaves no residue**, because both run
  before step 14 — so no zero-totals projection row is materialised for a
  jurisdiction- or exposure-rejected bet. Strictly better than the
  insufficient-funds decline, which does leave one (ADR 0082 §7's accepted
  consequence).

**No new lock-ordering violation is introduced.** The one ordering
inversion on this path (E-3) is pre-existing and is neither created nor
worsened by this ADR; it is named and guarded below because it was
previously unnamed.

### 7.3 Two amendments to ADR 0082

The implementer must make both edits to
`docs/decisions/0082-canonical-financial-lock-ordering.md` as part of this
wave, citing this ADR.

**Amendment A2 — new lock class L0.6.** ADR 0082 §2.1's table gains:

| Class | What | Within-class order |
| --- | --- | --- |
| L0.6 | **Sportsbook event-exposure advisory** (`sb_exposure:<tenant>:<event>`) — new, ADR 0083 §6.2.5 | one per bet; keyed on the event id so no ordering question arises |

It sits **after** L0.5 because the exposure gate runs after
`risk.Evaluate`, and because a single, consistent L0 sub-ordering is what
keeps a future reader from reversing them. `hashtextextended` for the full
64-bit key space, tenant-scoped, exactly like `AdvisoryLockGrant` and
`AdvisoryLockPlayerBonusScope`. Prospective binding: sportsbook
settlement, void, partial settlement and cashout, when built, must take
L0.6 before any L1/L2/L3 lock.

**Amendment A3 — new named exception E-3.** Alongside E-1 (§5.1) and E-2
(§5.1a):

> **E-3 — `sportsbook_bets` (L1) insertion wait taken after the L3
> pre-lock.** `sportsbook.PlaceBet` calls `insertBet` *after*
> `ledger.Post`, so the `UNIQUE (tenant_id, player_account_id,
> idempotency_key)` index insertion wait — a class L1 acquisition — happens
> after L3/L4. ADR 0082 §1.5 classified sportsbook bets as L1 but did not
> name this inversion.
>
> **Why it is not reordered:** `insertBet` must carry
> `ledger_transaction_id`, which does not exist until `Post` returns, and
> the cross-check immediately after it (the Stage 6.1 orphaned-posting
> guard) depends on comparing the two. Reordering means either splitting
> the insert into reserve/confirm or dropping that guard — the same shape
> of trade-off as E-2, and the same decision: name it, guard it, do not
> restructure the bet path under schedule pressure.
>
> **Why it is safe today:** a cycle needs a counterpart holding a
> `sportsbook_bets` row lock and then waiting on a
> `wallet_balance_projection` lock. None exists: `findBetByIdempotencyKey`,
> `/v1/me/sportsbook/bets` and the admin list are all unlocked reads, and
> settlement/void are NOT IMPLEMENTED. Two concurrent `PlaceBet`
> transactions take L3 then L1 in the *same* order as each other, which is
> consistent and therefore deadlock-free even though it is not canonical.
>
> **INV-LOCK-E3:** `sportsbook_bets` has exactly one writer
> (`sportsbook.insertBet`, from `PlaceBet`). Any second writer — in
> particular the settlement/void path — must resolve E-3 first, by taking
> its `sportsbook_bets` row lock at L1, *before* `LockProjectionsForPosting`.

### 7.4 Financial-invariant preservation

Both new gates are **pre-posting decision gates**, exactly like casino's
risk check. Neither writes a ledger entry, computes or reads a balance,
touches an idempotency key, alters `ledger.Post`'s inputs or outputs, or
changes what is posted. `SUM(DEBITS) == SUM(CREDITS)` is untouched. The
idempotency short-circuit remains strictly first, so a retry is still a
pure read. `ledger-finance` owns these invariants and may reject any part
of §6.2 that it judges to encroach on them; if it does, the outcome must
be recorded as an amendment here, not decided silently.

---

## 8. Human Decision Register

### 8.1 Workstream C needs NO new HDR entry

Stated explicitly so its absence is not read as an oversight. The
mechanism's existence requires no legal determination (ADR 0047 §3 already
established this), and its *population* is governed by controls that
already exist: platform-admin-only write, `authorization_reference` and
`reason_code` NOT NULL on every row, an audit record per mutation, and the
underlying legal questions already registered as HDR-J-6/7/8/9. Creating a
new item would duplicate them. Rung 2's blocker is **HDR-J-7**, an
existing item, and this ADR adds sportsbook to that item's
"what is blocked until answered" list.

### 8.2 HDR-SB-1 — Who carries the sportsbook trading-book liability, and must an exposure ceiling exist before go-live?

**Identifier:** HDR-SB-1.

**Exact question:** For the platform's own Anjouan-licensed B2C
sportsbook, does the platform itself carry the payout liability on
accepted bets — a trading book it owns and must cap — or will sportsbook
operate under a commercial arrangement in which a third-party provider
underwrites payouts, making the per-event/market/selection exposure
ceiling a provider-contract term rather than a platform-set number? If the
platform carries it: what is the per-`scope_kind`, per-asset ceiling on
aggregate open gross potential payout (§6.2.2's measure), and who owns
reviewing it?

**Why engineering cannot decide it:** the answer depends on a commercial
provider contract that does not exist (ADR 0080;
`docs/integrations/dummy-sportsbook.md` is still pending) and on the
operator's own risk appetite and solvency position. Any number engineering
chose would be an invented financial control, which CLAUDE.md forbids.
Choosing "no ceiling" is not a neutral default either — it is a decision
to accept unbounded trading-book exposure, which doc 09 §12 characterises
as "not a bounded engineering-bug cost, a trading-book loss". There is no
fail-safe direction engineering can pick.

**Affected domains:** `sportsbook`, `ledger-finance` (the Liability vs
Exposure boundary, doc 09 §1.7), `risk`, and the commercial/product owner.

**Options:** (a) the platform carries the book ⇒ a ceiling per
(`scope_kind`, `asset_code`) must be set, authorized and owned before
sportsbook go-live; (b) a provider underwrites payouts ⇒ the ceiling is a
contract term and the platform's own mechanism stays unarmed, or is armed
only as a defence-in-depth backstop at a number the contract implies;
(c) no sportsbook go-live until (a) or (b) is settled.

**Fail-closed / fail-open default until answered, stated honestly:** no
migration, seed or fixture creates any `sb_exposure_limits` row. With zero
rows the exposure gate is **unarmed and applies no check** — genuinely
fail-OPEN-when-unconfigured, identical to `risk_rules`,
`casino_games.jurisdiction_blocklist` and `operating_country_policies`,
and identical to today's behaviour, so this ADR introduces no new
permissiveness. The fail-CLOSED half is the other half: once a limit row
exists, **any** error reading it, any unscannable `NUMERIC`, any wrongly
scoped transaction, or any failure computing the aggregate aborts the bet
rather than allowing it.

**Security/regulatory impact if left open:** none today (the mechanism is
unarmed, the sportsbook runs on a mock provider with synthetic data, and
no real bet exists). If sportsbook goes live under option (a) without an
answer, the platform accepts unbounded per-outcome payout exposure with no
automated control.

**What is blocked until answered:** nothing in this ADR's implementation —
the mechanism ships unarmed. What is blocked is **sportsbook production
go-live**, which must not proceed without HDR-SB-1 being answered and, if
(a), a configured and authorized ceiling.

---

## 9. Implementation manifest

### 9.1 Migrations (expected 0087; take the next free number)

1. `CREATE TABLE sb_jurisdiction_restrictions` + RLS + three triggers
   (§5.2.2, §5.2.3).
2. `ALTER TABLE sportsbook_bets ADD COLUMN jurisdiction_code` +
   `CREATE OR REPLACE FUNCTION sportsbook_bets_enforce_immutable_fields`
   (§5.2.4).
3. `CREATE TABLE sb_exposure_limits` + RLS + triggers, and
   `CREATE INDEX idx_sportsbook_bets_open_exposure` (§6.2.3).

No statement in this migration touches `casino_games`, `sb_sports`,
`sb_competitions`, `sb_events`, `sb_markets` or `sb_selections`. A
`.down.sql` must exist and must drop in reverse order; dropping the
`sportsbook_bets` column is reversible (no ledger row depends on it).

### 9.2 New Go files

- `internal/sportsbook/jurisdiction.go` — §5.4's surface.
- `internal/sportsbook/exposure.go` — §6.2.4's surface.
- `internal/sportsbook/jurisdiction_admin.go` /
  `exposure_admin.go` — create/withdraw/list for the two tables, modelled
  **exactly** on `internal/risk/policy_service.go` + `risk_handlers.go`
  (audited, reason-coded, permission-gated). Platform-admin scope for
  restrictions; tenant `risk_manager`-class scope for exposure limits.
  New HTTP routes under `/v1/admin/sportsbook/jurisdiction-restrictions`
  and `/v1/admin/sportsbook/exposure-limits`, with the corresponding
  OpenAPI additions.

### 9.3 Changed Go files

- `internal/risk/cumulative.go` — one map entry (§6.1.1); update the
  doc-comment reference block to stop naming `sportsbook_void` and to
  point at this ADR.
- `internal/sportsbook/orchestrator.go` — steps 5–7, 9 (`JurisdictionCode`),
  10 (moved), 11, 17 (`jurisdiction_code`), 18 (audit metadata).
- `internal/sportsbook/catalogue.go` — the two annotators (§5.4.1).
- `internal/sportsbook/types.go` — `Available`/`UnavailableReason` on
  `EventSummary`/`MarketDetail`/`Selection`; new `RejectionJurisdictionDenied`
  and `RejectionExposureLimit` constants; new `DenialCode*` constants.
- `internal/sportsbook/bets.go` — `insertBetParams.JurisdictionCode`.
- `internal/httpserver/sportsbook_handlers.go` — pass an
  `AvailabilityContext` on authenticated catalogue reads; anonymous reads
  pass the zero value. Collapse both jurisdiction denial codes into one
  player-facing message (K3-6). Never surface an exposure amount or
  threshold.
- `docs/decisions/0082-…md` — Amendments A2 and A3 (§7.3).
- `docs/architecture/09-sportsbook-architecture.md` — §3.4 gains the
  Open-Question-3 answer (§6.2.1); §16.3's `player_locked`/`sportsbook_void`
  drift notes are marked resolved by this ADR.
- `docs/governance/task-registry.md` — `SB-JUR-RUNG2-1`, HDR-SB-1, and
  the ADR 0047 items now closed.

### 9.4 Wave split (sequencing advice, not a requirement)

- **Wave 1 (Part B1):** the `operationCumulativeSpecs` entry + its tests.
  Self-contained, no migration, independently correct. **Do not land it
  before Wave 2** unless the `ErrMissingJurisdiction` lever (§6.1.5) is
  separately accounted for.
- **Wave 2 (Part C):** migration items 1–2, `jurisdiction.go`, the
  orchestrator steps 5–7/9/17/18, the catalogue annotators, the admin
  surface. This is the wave `security` must review.
- **Wave 3 (Part B2):** migration item 3, `exposure.go`, orchestrator
  steps 10–11, the admin surface, ADR 0082 Amendments A2/A3.

Waves 2 and 3 both edit §7's step list; landing them out of order is fine,
landing them concurrently is not.

## 10. Architectural invariants (binding on `qa` and `code-reviewer`)

- **INV-SB-JUR-1** — There is exactly ONE implementation of "may this
  player bet on this selection". `PlaceBet` and both catalogue annotators
  call the same `ResolvePlayerJurisdiction` +
  `evaluateJurisdictionRestriction` pair. A second, divergent copy is a
  defect. Verifiable by grep: `jurisdiction.Resolve` appears in
  `internal/sportsbook` exactly once.
- **INV-SB-JUR-2** — `sb_jurisdiction_restrictions` is deny-only. No row
  in it can cause a bet to be accepted that would otherwise be rejected.
  Structurally enforced by `CHECK (restriction_kind = 'blocked')`.
- **INV-SB-JUR-3** — No client-supplied jurisdiction, country, or
  availability answer is ever trusted. `PlaceBetParams` and every
  catalogue request type carry no such field, and
  `jurisdiction.Resolution` has no exported constructor.
- **INV-SB-JUR-4** — A tenant-licence jurisdiction can never become a
  player's jurisdiction on this path: `Resolve` is always called with a
  non-nil `PlayerAccountID`, for which `BasisTenantLicence` is
  structurally unreachable (`resolver.go` §3.2). BYOL and player
  jurisdiction stay separate by construction, not by review.
- **INV-SB-JUR-5** — The licence ceiling is never widened. Rung 3 only
  narrows; rung 2, when implemented, treats every
  `operatingmarket.Outcome` other than `OutcomePermitted` as a denial and
  is never bypassable by tenant or brand configuration.
- **INV-SB-JUR-6** — A placed bet's `jurisdiction_code` is immutable
  (trigger-enforced) and is never recomputed by any later reader.
- **INV-SB-CUM-1** — Any migration widening
  `ledger_transactions.transaction_type` with a sportsbook reversal type
  must update `operationCumulativeSpecs[OperationSportsbookBet].ReversalTypes`
  in the same change.
- **INV-SB-EXP-1** — Sportsbook exposure is never aggregated across
  tenants. Structurally guaranteed by `sportsbook_bets`' FORCE RLS and
  `PlaceBet`'s tenant-scoped transaction; a cross-tenant figure would
  require breaking tenant isolation and is forbidden.
- **INV-SB-EXP-2** — Exposure is never treated as a balance, never
  written to the ledger, and never surfaced to a player. No amount or
  threshold from `evaluateExposureLimits` reaches an HTTP response body.
- **INV-LOCK-E3** — see §7.3.
- **INV-SB-ORDER-1** — Every decision gate in `PlaceBet` runs before
  `ledger.LockProjectionsForPosting`. A gate added after it is a defect,
  whatever it checks.

## 11. Known asymmetries and follow-ups (recorded, not built)

- **Casino has no operating-market rung and no catalogue-visibility
  marking.** After this ADR, sportsbook will be stricter than casino in
  both respects once rung 2 lands. That is the safe direction, but the
  asymmetry should be closed by bringing casino up, not sportsbook down —
  registered as a follow-up, not built here.
- **`casino_games.jurisdiction_blocklist` carries no per-restriction
  authorization_reference/reason_code.** §5.2.1 point 4's weakness exists
  in casino today. A future migration could move casino onto a table of
  the same shape as `sb_jurisdiction_restrictions`. Not in scope.
- **ADR 0047 §5's two remaining deferrals** (the general cumulative-rule
  write-time validation gap in `risk.CreateRule`; the missing explicit
  `PrincipalType == player` assertion on `/v1/me/sportsbook/bets`) are
  unchanged by this ADR and remain open.
- **Exposure netting (`potential_return − stake`), per-instance ceilings,
  and finer-grained exposure locking** are named future refinements, each
  requiring an amendment here.

## 12. Test plan — by name and intent, for `qa` to implement

This ADR does not write these tests. Every concurrency test must be
**deterministic** — a blocker transaction holding a known lock plus
`pg_stat_activity` lock-wait polling, the pattern
`internal/casino/stage9_concurrency_integration_test.go` and
`internal/risk/cumulative_race_integration_test.go` already use — never a
bare goroutine race that passes by luck.

### 12.1 Part C

1. `TestSportsbookJurisdiction_AllowedWhenNoRestrictionConfigured` —
   K3-1: unarmed control never denies, even though every player-scoped
   resolution is `unresolved(no_signal)` today. **This is the test that
   proves this ADR did not break every sportsbook bet on the platform.**
2. `TestSportsbookJurisdiction_RestrictedSelectionDeniesMatchingJurisdiction` —
   armed + resolved + code in the restricted set ⇒
   `jurisdiction_blocked`.
3. `TestSportsbookJurisdiction_BlockedAtEventLevelBlocksEveryMarketAndSelectionBeneath` —
   downward propagation; and its negative, that a restricted selection
   does not block its sibling selections or its market.
4. `TestSportsbookJurisdiction_UnresolvedFailsClosedWhenArmed` — K3-2:
   armed + non-`Resolved` ⇒ `jurisdiction_unresolved`, **never**
   `jurisdiction_blocked`, asserted on the distinct code.
5. `TestSportsbookJurisdiction_ExpiredEvidenceFailsClosed` — asserts the
   fail-closed posture for stale/expired jurisdiction evidence. Today this
   is reachable only through `jurisdiction.DeterminePlayerJurisdiction`'s
   `MaxLocationSignalAge` path, which has no production caller; the test
   is written against the evaluation function's contract and is marked
   **BLOCKED on HDR-J-7** in the suite rather than silently omitted.
6. `TestSportsbookJurisdiction_MissingConfigurationIsNotAWildcard` — a
   restriction row naming a `jurisdiction_code` that does not exist in
   `jurisdictions` is refused by the FK at write time; a withdrawn row is
   never evaluated.
7. `TestSportsbookOperatingMarket_LicenceCeilingDenialBlocksPlacement` —
   rung 2. **BLOCKED on HDR-J-7**; written against
   `evaluateOperatingMarket`'s §5.3.3 contract so it compiles into the
   suite the day the rung lands.
8. `TestSportsbookOperatingMarket_TenantScopeDisableBlocksPlacement` — as
   above, tenant rung.
9. `TestSportsbookOperatingMarket_BrandScopeDisableBlocksPlacement` — as
   above, brand rung.
10. `TestSportsbookOperatingMarket_OperationScopeDisableBlocksPlacement` —
    as above, `wagering`/`sportsbook` operation+product rung; and that a
    tenant/brand row can never widen past a denial above it.
11. `TestSportsbookJurisdiction_CrossTenantRestrictionAndBetIsolation` —
    tenant A's bet is unaffected by tenant B's configuration; a
    tenant-scoped connection cannot read or write another tenant's
    `sb_exposure_limits`; the platform-scoped restriction table applies
    identically to both.
12. `TestSportsbookJurisdiction_DirectAPIBypassIsRejected` — POST
    `/v1/me/sportsbook/bets` with a restricted `selection_id`, never
    having called the catalogue, is rejected with the same decision the
    catalogue would have marked.
13. `TestSportsbookJurisdiction_ConcurrentConfigurationReadIsConsistent` —
    a restriction inserted concurrently with an in-flight `PlaceBet`
    yields exactly one coherent outcome; the bet either sees the
    restriction or does not, never a torn read, and a rejected bet leaves
    no ledger effect and no projection row.
14. `TestSportsbookCatalogue_RestrictedRowIsMarkedUnavailableNotOmitted` —
    the §5.4.1 decision, asserted on the response shape.
15. `TestSportsbookCatalogue_AnonymousReadIsUnchanged` — zero-value
    `AvailabilityContext` ⇒ byte-identical response to today's.
16. `TestSportsbookCatalogue_AndPlaceBetShareOneEvaluation` — a static
    source test asserting `jurisdiction.Resolve` appears exactly once in
    `internal/sportsbook` (INV-SB-JUR-1's regression guard — the one test
    that keeps this ADR from decaying into two implementations).
17. `TestSportsbookBet_JurisdictionSnapshotIsImmutable` — the placed bet's
    `jurisdiction_code` cannot be UPDATEd, and changing the restriction
    configuration afterwards does not alter it (§5.5).
18. `TestSportsbookRestrictions_WriteRequiresPlatformAdminScope` — RLS
    proof: tenant-scoped, player-scoped, catalogue-sync-service-scoped and
    bare connections all fail to write; platform-admin succeeds and the
    audit record carries the authorization reference.
19. `TestSportsbookCatalogueTables_WriteAuthorizationUnchanged` — a
    regression guard that migration 0084/0085's policies on all six
    catalogue tables are byte-identical after this migration.

### 12.2 Part B

20. `TestSportsbookCumulative_SimultaneousSamePlayerBetsCannotBothPass` —
    two concurrent `PlaceBet` calls for one player against a
    `cumulative_amount` rule only one can fit under; exactly one is
    accepted. Must be shown to fail without the L0.5 lock.
21. `TestSportsbookExposure_CrossPlayerBetsOnOneSelectionAreAggregated` —
    two different players, same selection, same tenant; the second is
    rejected once the aggregate crosses the ceiling.
22. `TestSportsbookExposure_EventMarketAndSelectionLevelsEachEnforce` —
    three sub-cases, one per `scope_kind`, including that an event-level
    ceiling aggregates bets placed on *different* selections beneath it.
23. `TestSportsbookExposure_TenantIsolation` — tenant B's open bets on the
    same platform-wide selection do not count toward tenant A's aggregate
    (INV-SB-EXP-1); and a brand-scoped limit does not aggregate a sibling
    brand's bets.
24. `TestSportsbookExposure_BalanceInteraction` — an exposure rejection
    happens before `LockProjectionsForPosting`, so it (a) leaves the
    player's balance untouched, (b) materialises **no** zero-totals
    projection row, and (c) still commits its audit record.
25. `TestSportsbookExposure_RejectionShapeLeaksNoAmounts` — the rejection
    result and the HTTP body carry no aggregate, no threshold and no limit
    id (INV-SB-EXP-2).
26. `TestSportsbookCumulativeAndExposure_IdempotentRetryReevaluatesNothing` —
    a retry with the same idempotency key returns the original bet without
    re-running the jurisdiction, RG, risk or exposure gates and without
    taking L0.4/L0.5/L0.6. Assert on lock acquisition, not only on the
    result.
27. `TestSportsbookCumulative_RollbackOfTheWholeTransactionReleasesEveryLock` —
    an error after the exposure gate rolls back the bet, the ledger
    posting and the audit record together, and releases L0.4/L0.5/L0.6.
28. `TestSportsbookCumulative_VoidedBetStillConsumesCapacityUntilReversalTypeExists` —
    pins §6.1.2's conservative, disclosed over-count so it is a decision
    on the record rather than a surprise; must be updated, not deleted,
    when the reversal type lands.
29. `TestSportsbookExposure_SettlementInteractionIsSpecifiedNotImplemented` —
    asserts `status` values other than `'open'` are excluded from the
    aggregate, using directly-inserted rows, since no settlement path
    exists to produce them.
30. `TestLockOrder_SportsbookPlaceBetAcquiresLocksInCanonicalOrder` — the
    §7.2 sequence asserted directly (L0.4 → L0.5 → L0.6 → L3 → L4 → E-3),
    plus `TestLockOrder_ConcurrentSportsbookBetsOnSameEventNoDeadlock` and
    `TestLockOrder_ConcurrentSportsbookBetAndCasinoBetSameWalletNoDeadlock`.
31. `TestSportsbookCumulative_FailsClosedOnEveryUnmeasurableConfiguration` —
    each of `ErrUnsupportedCumulativeOperation`, `ErrInvalidCumulativeSpec`,
    `ErrUnrecognizedCumulativeLeg`, a conflicting-rule configuration, and
    a wrongly scoped transaction aborts the bet; **none** is reported as a
    decline and none as an allow.
32. `TestSportsbookExposure_FailsClosedOnUnreadableOrUnscannableLimit` — a
    `NUMERIC` outside the scannable range, a player-scoped transaction,
    and a database error each abort the bet.
33. `TestSportsbookCumulative_TwoPlayerOwnedLegsAreNotNettedToZero` — the
    sportsbook-specific instance of ADR 0031 §32(a)'s defect: a
    `cumulative_amount` rule over `Dr player_cash / Cr player_locked_cash`
    measures the full stake, and is shown to net to zero if
    `IgnoredAccountTypes` is emptied.

### 12.3 Must continue to pass unchanged

The whole existing `internal/sportsbook` suite — in particular
`orchestrator_integration_test.go`'s idempotency, odds-changed,
insufficient-funds and RG/risk decline cases,
`failure_mode_matrix_integration_test.go`,
`migration_0082_immutability_integration_test.go`,
`catalogue_write_authorization_integration_test.go` — plus ADR 0082 §6's
entire lock-ordering suite, with no weakening of any assertion.

## 13. Related

- `docs/decisions/0006` (hybrid licensing), `0031` (risk & limits engine),
  `0038` (sportsbook accounting), `0041`–`0045` (jurisdiction HDR,
  evaluation policy, operating market), `0046` (tenant licence RLS),
  `0047` (this ADR's baseline), `0080` (provider readiness without
  contracts), `0081` (ARCH-DB-2 catalogue write authorization), `0082`
  (canonical financial lock ordering — amended here).
- `docs/architecture/09-sportsbook-architecture.md` §1.7, §3.4, §16.3,
  Open Question 3; `docs/architecture/ledger-accounting-model.md` §6.4/HR-8.
- Migrations `0041` (risk_rules), `0042` (casino launch-session
  jurisdiction snapshot), `0048` (locked-origin split), `0076`
  (operating market), `0078` (sportsbook foundation), `0082` (sportsbook
  bet immutability), `0084`/`0085` (catalogue write authorization — **not
  modified by this ADR**).
