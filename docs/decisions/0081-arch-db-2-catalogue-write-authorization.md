# ADR 0081 — Database-Level Write Authorization for the Six Platform-Wide Catalogue Tables (closes `ARCH-DB-2`)

**2026-09-21 renumbering note (backend-engineer, implementing wave):** a parallel Stage 9.1 devops workstream (`PLAT-MIGDRIFT-1`) landed `migrations/0083_migration_checksum_tracking` first, so every "migration `0083`" reference below that names the migration this ADR specifies is corrected to "migration `0084`" throughout this document - a pure renumbering pass, no design content changed.

Status: **Accepted (design ruling only — NOT IMPLEMENTED).**

*Status note 2026-09-26: this header is historical. The ruling was implemented in Stage 9.1 (ARCH-DB-2 closed) by migration `0084_catalogue_write_authorization` with fix-round migration `0085_casino_games_require_platform_principal`; §5.2 (casino four-eyes) was implemented in Stage 9.2 by migrations `0086_casino_catalogue_dual_control` and `0089_casino_catalogue_governance_hardening`. See `docs/progress.md` "Stage 9.1" and "Stage 9.2".*

Owner: `architect`. Requires `security` review before the implementing
wave's output is marked complete (CLAUDE.md, "Security"). Financial
invariants touched are read-only observations about `sb_selections.odds_*`
and `sportsbook_bets`; nothing here changes a ledger rule, so
`ledger-finance` is a reviewer, not a co-decider.

Stage: 9.1. Supersedes nothing. Refines (does not replace) ADR 0014 §1.

**Numbering note.** The highest existing ADR in `docs/decisions/` is
`0080`, so this ruling is `0081`. The dispatch that commissioned it
proposed the filename `0083`, which is the next *migration* number, not
the next ADR number. The two sequences are independent in this repo
(ADR 0080 sits beside migration 0082). This ADR is `0081` and it
specifies migration `0084` (see the 2026-09-21 renumbering note above).

---

## 1. Context

The Stage 9 architect review (`S9-12`) raised `ARCH-DB-2` (HIGH, FIX
BEFORE PRODUCTION) and deferred it as cross-domain. Six tables have no
row-level security at all — no `ENABLE ROW LEVEL SECURITY`, no policies,
no write-side triggers:

| Table | Introduced by | Production writer today |
|---|---|---|
| `casino_games` | migration `0035` §2 | `casino.UpsertGame`, called only from `newUpsertCasinoGameHandler` |
| `sb_sports` | migration `0078` §2 | `sportsbook.SyncCatalogue` → `upsertSport` |
| `sb_competitions` | migration `0078` §2 | `sportsbook.SyncCatalogue` → `upsertCompetition` |
| `sb_events` | migration `0078` §2 | `sportsbook.SyncCatalogue` → `upsertEvent` |
| `sb_markets` | migration `0078` §2 | `sportsbook.SyncCatalogue` → `upsertMarket` |
| `sb_selections` | migration `0078` §2 | `sportsbook.SyncCatalogue` → `upsertSelection` |

Verified by exhaustive grep across `internal/` and `cmd/`: those are the
*only* production write paths. The remaining writers are integration-test
fixtures (§7.4). There is no seed tool, no back-office writer, and no
provider callback that mutates any of the six.

Why it matters concretely, restated from the actual code rather than
asserted:

* `sb_selections.odds_numerator` / `odds_denominator` are copied onto
  `sportsbook_bets` at acceptance and frozen there (migration `0078`'s own
  column comment; migration `0082` §1.1 added the trigger that freezes
  them on the bet). The freeze protects *already-placed* bets. It does
  **not** protect the window before placement: a write path that could
  reach `sb_selections` could misprice a live selection, or flip
  `sb_selections.status` / `sb_markets.status` / `sb_events.status` back
  to a bettable value, and `PlaceBet`'s structural validation
  (`getSelectionWithContext`, `internal/sportsbook/catalogue.go:307`)
  would faithfully accept the result. That is real money, today.
* `casino_games.jurisdiction_blocklist` is, in this platform's own prior
  words (`SEC-4I-F3`, quoted in `casino_admin_handlers.go:52-58`), "a
  live, platform-wide, single-actor, non-four-eyes denial control the
  moment this handler adds a code to it". It is enforced at launch by
  `internal/casino/orchestrator.go`'s `evaluateJurisdictionBlocklist` —
  a wired, tested enforcement point, unlike sportsbook, which has no
  equivalent column at all.

The platform already solved the analogous problem for `assets`
(migrations `0044`/`0045`, ADR 0037 Part C). This ADR decides whether
that precedent transfers, and what exactly to build.

---

## 2. Per-table disposition

### 2.1 Why all six are currently RLS-free — verified, not assumed

I read the actual `CREATE TABLE` statements. **None of the six carries a
`tenant_id` column, a `brand_id` column, or any other ownership column.**
`casino_games` is `(id, provider_id, provider_game_id, name, game_type,
rtp_variant, volatility, feature_flags, supported_assets,
mobile_supported, demo_supported, jurisdiction_blocklist, status,
created_at, updated_at)`. The five `sb_*` tables are
`(id, external_ref, …parent_id…, name, [status|odds|start_time],
created_at, updated_at)`.

So the reason is exactly the one the dispatch hypothesised, and both
migrations say so in their own header comments: these are **platform-wide
catalogue/reference data**. There is one canonical `casino_games` row per
provider title and one canonical `sb_events` row per real-world fixture,
visible to every tenant. There is no tenant or brand to scope by, so the
codebase's standard `tenant_id = current_setting('app.tenant_id')` policy
shape is inexpressible — and, lacking any other model, the tables were
left with no RLS at all rather than with a different kind of RLS. That
gap is the finding.

### 2.2 Can tenant/brand ownership be represented at the database layer?

**No — and adding `tenant_id`/`brand_id` to any of the six would be
semantically wrong. This is a rejection, not a deferral.**

For `casino_games`:

* Per-tenant *availability* already exists as its own correctly-modelled,
  RLS-protected, tenant-owned table: `casino_game_availability`
  (`tenant_id NOT NULL`, nullable `brand_id`, `FORCE ROW LEVEL SECURITY`,
  `tenant_isolation` policy — migration `0035` §3). Resolution is
  fail-closed: `IsGameAvailable` returns `false` when no row exists, so a
  tenant never inherits catalogue membership implicitly.
* A `tenant_id` on `casino_games` would duplicate one title into N rows,
  which destroys the `UNIQUE (provider_id, provider_game_id)` constraint
  that *is* the provider's own identity for that title (ADR 0025 §2), and
  silently re-scopes `casino_launch_sessions.game_id`,
  `casino_provider_rounds.game_id` (migration `0080`) and
  `risk_rules.game_id` (migration `0041`), all of which point at the
  single canonical row.
* It would also dissolve the platform-vetting boundary that
  `PermCasinoCatalogueManage` exists to draw
  (`internal/auth/permission.go:107-113`): "a tenant may opt into a title
  the platform has already vetted but must never be able to add an
  unvetted title to the shared catalogue every other tenant can then also
  see." A tenant-owned catalogue row is precisely the thing that
  permission forbids.

For the five `sb_*` tables:

* `sportsbook_bets.selection_id` is a foreign key to the *canonical*
  selection. Forking selections per tenant would make the same provider
  price diverge per tenant with no reconciliation path — a trading
  integrity hazard, and one the platform has no mechanism to detect.
* Sportsbook has no per-tenant availability layer today. That is a known,
  disclosed scope simplification (`internal/httpserver/sportsbook_handlers.go:23-28`,
  `internal/sportsbook`'s package doc). When it is built, the correct
  shape is a **new tenant-owned table** mirroring
  `casino_game_availability` (working name `sb_event_availability` or
  `sb_competition_availability`), with its own `tenant_id`, its own
  `FORCE` RLS and its own `tenant_isolation` policy — not a `tenant_id`
  column bolted onto `sb_events`. Recorded here so a future stage does
  not reopen this as a fresh question; it is **out of scope for this ADR
  and NOT authorized by it**.

**Ruling: the correct backstop for all six is not per-tenant RLS. It is
`ENABLE` + `FORCE ROW LEVEL SECURITY` with read-open SELECT and a write
policy scoped to a platform-level identity — the `assets` shape
(migration `0044` §3), adapted where the semantics differ (§2.5).**

### 2.3 Exact READ requirement

| Table | Anonymous/unauthenticated read required? | Evidence |
|---|---|---|
| `sb_sports` | **Yes** | `GET /v1/sportsbook/sports` is mounted with `mux.HandleFunc`, no `auth.Middleware` (`sportsbook_routes.go:23`), running under `WithoutTenant` (`sportsbook_handlers.go:81`) |
| `sb_competitions` | **Yes** | same handler (`ListSportsCatalogue`) |
| `sb_events` | **Yes** | same handler, plus `GET /v1/sportsbook/events/{id}` (`sportsbook_routes.go:24`, `sportsbook_handlers.go:165`) |
| `sb_markets` | **Yes** | `GetEventDetail`, same public route |
| `sb_selections` | **Yes** | `GetEventDetail`, same public route (this is the betslip price feed) |
| `casino_games` | **No** | every reader is authenticated: `ListAvailableGames` and `LaunchGame` under `WithTenant`, before-image read under `WithoutTenant` |

All six additionally need reads from `WithTenant` (bet placement's
`getSelectionWithContext`; `ListAvailableGames`; `LaunchGame`;
`ReceiveCallback`'s `GetGameByID` at `internal/casino/orchestrator.go:979`).
No current reader runs under `WithPlayerScope`, but nothing should depend
on that staying true.

**Ruling: `FOR SELECT USING (true)` on all six**, including
`casino_games` even though it has no anonymous reader today. Rationale,
stated positively rather than as a shrug: restricting SELECT adds no
protection against the finding (which is about writes), PostgreSQL
bypasses RLS for referential-integrity checks regardless — five tables
carry inbound FKs from `sportsbook_bets`, `casino_game_availability`,
`casino_launch_sessions`, `casino_provider_rounds` and `risk_rules` — and
a narrower predicate would make `ListAvailableGames`' join fragile under
any future scope change for zero benefit. This is `assets_read`'s exact
precedent and reasoning (migration `0044` §3).

**One honest, accepted consequence:** open SELECT on `casino_games`
exposes `jurisdiction_blocklist` to any connection, including
unauthenticated ones if a public catalogue route is ever added. Today
every blocklist is empty (no `HDR-J` item has been answered, so no
country is blocked anywhere — `internal/sportsbook/orchestrator.go:104-107`).
If populated blocklists are later judged operator-sensitive, narrowing
`casino_games`' SELECT is a separate, small, reversible decision. It is
**not** made here.

### 2.4 Exact WRITE requirement

| Table | Legitimate writer | Principal shape |
|---|---|---|
| `casino_games` | `newUpsertCasinoGameHandler` only | A **human platform admin**, request-scoped, already gated by `PermCasinoCatalogueManage` (platform-only, never `RoleTenantAdmin`) |
| `sb_sports` … `sb_selections` | `sportsbook.SyncCatalogue` only | A **background process at startup with no authenticated principal at all** |

Never, for any of the six: an end user, a player-scoped connection, a
tenant-scoped connection, ordinary tenant staff, or a provider webhook.

These two writers need *different* identities. That is the crux of the
design, and §3.2 settles it.

### 2.5 Where the `assets` precedent does NOT transfer — verified

The dispatch asked me to check the precedent rather than copy it. Two
parts of migrations `0044`/`0045` are deliberately **not** carried over:

1. **`assets`' fail-closed creation defaults and four-eyes-on-create.**
   `assets` dual-controls *creation* because an asset row plus activation
   directly authorizes financial operations (ADR 0037 §C.5.5). A
   `casino_games` row authorizes nothing on its own: every tenant must
   still write a `casino_game_availability` opt-in row, and
   `IsGameAvailable` fails closed without one. Requiring four-eyes to
   register a title would therefore be friction with no control behind
   it. **Creation stays single-actor + audited.**
2. **Freezing value-bearing columns.** `assets.decimal_exponent` is frozen
   because changing it reinterprets every historical balance.
   `sb_selections.odds_*` look analogous and are **not** frozen here:
   updating live odds is the catalogue sync's entire job, already-placed
   bets are protected by the acceptance-time freeze on `sportsbook_bets`,
   and a trigger forbidding odds updates would break the only legitimate
   writer. Identity is frozen; price is not (§4.2).

---

## 3. The enforcement design

### 3.1 The shared shape (all six tables)

```sql
ALTER TABLE <t> ENABLE ROW LEVEL SECURITY;
ALTER TABLE <t> FORCE ROW LEVEL SECURITY;
```

`FORCE` is mandatory, not stylistic. Dev, CI and every environment that
has not completed `PLAT-ROLESPLIT-1`'s production cutover still connect
as the table-owning `igaming` role, and RLS never applies to a table's
owner without `FORCE` (`docs/security/runtime-role-separation.md` §1).
Without `FORCE`, the policies below would be inert in exactly the
environments where they must be testable.

Honest limitation, restated from migration `0044`'s own admission rather
than hidden: a policy keyed on a session GUC binds a *code path*, not an
OS boundary — code that can execute arbitrary SQL on the application
connection can call `set_config`. What it does close, completely, is the
finding as written: today **any** connection can write these six tables;
after this change, only a connection that has deliberately entered a
named platform scope can, and tenant-scoped and player-scoped
connections are *structurally* unable to, because they always set
`app.tenant_id` / `app.player_account_id` and every write predicate below
requires those to be unset.

### 3.2 Two write identities, and why the second one is new

**`casino_games` → the existing `app.platform_admin_principal_id` GUC**,
set only by `db.Pool.WithPlatformAdmin`. This is a request-scoped human
platform admin resolved from a verified token subject — exactly what that
helper documents itself as (`internal/db/tenant_rls.go:74-98`). No new
concept needed.

**The five `sb_*` tables → a new, narrowly-scoped service identity.**
The sportsbook catalogue sync runs once at startup
(`cmd/platform-api/main.go:152-157`) with no HTTP request, no token, and
no principal. Three candidates were considered:

* *(a) Keep `WithoutTenant`.* **Rejected.** `WithoutTenant` is the same
  scope every ordinary platform read already uses (tenant lookup, brand
  lookup, session lookup, the public catalogue browse itself). A write
  policy keyed on it would grant nothing and would be security theatre —
  it would let the public read handler at `sportsbook_handlers.go:81`
  write the catalogue. ADR 0014 §1 named `WithoutTenant` as the platform
  service-identity pattern; that remains correct for *reads*, and is
  exactly what is insufficient here. This ADR refines ADR 0014 §1; it
  does not overturn it.
* *(b) Call `WithPlatformAdmin` with a synthetic UUID.* **Rejected.** It
  would launder a machine process as a human platform-admin principal,
  contradicting that helper's own contract ("must be the platform-scoped
  staff principal resolved from the verified token's own subject"), and
  would make any audit or forensic read of that GUC actively misleading.
* *(c) A new, closed-vocabulary service scope.* **Chosen.**

**New helper — `internal/db/platform_service.go` (new file):**

```go
// PlatformService is the closed vocabulary of non-human, non-request-scoped
// platform processes that are permitted to write platform-wide catalogue
// data. Adding a member is an ADR-level decision plus a migration that
// widens the corresponding policy predicate - never a bare string at a
// call site.
type PlatformService string

const ServiceSportsbookCatalogueSync PlatformService = "sportsbook_catalogue_sync"

// WithPlatformService runs fn in a genuinely platform-scoped transaction
// (app.tenant_id and app.player_account_id deliberately never set) with
// "app.platform_service_id" set, for the lifetime of the transaction, to
// service. [...]
func (p *Pool) WithPlatformService(ctx context.Context, service PlatformService, fn TxFunc) error
```

Required behaviour, exactly:

1. Reject `service == ""` with `db: WithPlatformService called with empty
   service identity`.
2. Reject any value absent from an unexported package-level
   `map[PlatformService]struct{}` allowlist containing exactly
   `ServiceSportsbookCatalogueSync` today, with
   `db: WithPlatformService called with unknown service identity %q`.
   The allowlist is the point: the GUC value can never be attacker- or
   config-supplied.
3. `BEGIN`; `SELECT set_config('app.platform_service_id', $1, true)`
   (transaction-local, bound parameter — never interpolated), run `fn`,
   commit or roll back. Structurally identical to `WithPlatformAdmin`.
4. Set **nothing else**. Never `app.tenant_id`, never
   `app.player_account_id`, never `app.platform_admin_principal_id`.

`internal/db/platform_service.go` must also export, for in-Go
belt-and-braces assertions mirroring `internal/jurisdiction`'s
`assertPlatformScope`:

```go
var ErrPlatformServiceScope = errors.New("db: requires a platform-service-scoped transaction (use db.Pool.WithPlatformService)")

// AssertPlatformServiceScope verifies, from inside an already-open tx, that
// app.platform_service_id equals service and that app.tenant_id /
// app.player_account_id are both unset.
func AssertPlatformServiceScope(ctx context.Context, tx pgx.Tx, service PlatformService) error
```

### 3.3 Exact policy specification

Policy names follow migration `0077`'s convention (`<table>_read`,
`<table>_platform_admin_<verb>`).

**`casino_games` — three policies.**

```sql
CREATE POLICY casino_games_read ON casino_games
    FOR SELECT USING (true);

CREATE POLICY casino_games_platform_admin_insert ON casino_games
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY casino_games_platform_admin_update ON casino_games
    FOR UPDATE
    USING (<same predicate>)
    WITH CHECK (<same predicate>);

CREATE POLICY casino_games_platform_admin_delete_visibility ON casino_games
    FOR DELETE
    USING (<same predicate>);
```

The DELETE policy grants **visibility only**; the deny-DELETE trigger
(§4.3) is what refuses. This is migration `0044`'s
`assets_platform_admin_delete_visibility` pattern and exists for its
stated reason: with no DELETE policy at all, RLS makes every row
invisible to a DELETE, so the statement silently affects zero rows
instead of failing loudly — and a `casino_games` row is referenced by
launch sessions, provider rounds and risk rules, where a silent no-op is
a worse answer than a loud refusal.

**Each of `sb_sports`, `sb_competitions`, `sb_events`, `sb_markets`,
`sb_selections` — four policies each, with `<t>` substituted.**

```sql
CREATE POLICY <t>_read ON <t>
    FOR SELECT USING (true);

CREATE POLICY <t>_catalogue_sync_insert ON <t>
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY <t>_catalogue_sync_update ON <t>
    FOR UPDATE
    USING (<same predicate>)
    WITH CHECK (<same predicate>);

CREATE POLICY <t>_catalogue_sync_delete_visibility ON <t>
    FOR DELETE
    USING (<same predicate>);
```

Note the predicate is a **string equality against the literal
`'sportsbook_catalogue_sync'`**, not merely "is set" — so a future second
service identity does not silently inherit write access to the sportsbook
catalogue. Widening requires a migration that names the new value.

**Deliberately NOT granted: platform-admin write on the five `sb_*`
tables.** There is no admin HTTP write path for them today, and
pre-granting a capability with no handler would create an unaudited write
surface, violating CLAUDE.md's "every mutating administrative action
writes an audit record". When an operational correction path (e.g.
emergency market suspension) is actually built, it adds its own policy in
its own migration alongside its own audited handler. That friction is
intended.

**`ON CONFLICT … DO UPDATE` is unaffected.** Both writers upsert. Under
RLS an upsert needs INSERT `WITH CHECK`, UPDATE `USING` + `WITH CHECK`,
and SELECT visibility of the conflicting row; all four are supplied
above. Referential-integrity checks from `sportsbook_bets`,
`casino_game_availability`, `casino_launch_sessions`,
`casino_provider_rounds` and `risk_rules` are unaffected because
PostgreSQL's RI triggers bypass RLS on the referenced table regardless of
`FORCE` — migration `0044` already relies on exactly this for `assets`.

### 3.4 Migration-time and incident escape hatch (binding rule)

Once `FORCE` is on, the migration-owner role is subject to these policies
too. A future migration or an operator that needs to correct data in any
of the six **must** set the appropriate GUC inside its own transaction:

```sql
SELECT set_config('app.platform_service_id', 'sportsbook_catalogue_sync', true);   -- sb_* tables
SELECT set_config('app.platform_admin_principal_id', '<a real platform staff uuid>', true);  -- casino_games
```

It **must not** toggle `NO FORCE`/`FORCE ROW LEVEL SECURITY` around the
statement. That pattern was found blocking by `security` as finding `S-1`
(migration 0048, recorded in `docs/active-stage.md`): the restore is
transaction-local, so a standalone `psql -f` run leaves the table with
protection silently and permanently off.

Within migration `0084` itself, any data statement touching the six
tables must appear **before** the `ENABLE`/`FORCE` statements, exactly as
migration `0044` ordered its seeded-`assets` `UPDATE` before enabling RLS.
Migration `0084` as specified here has no such data statement.

---

## 4. Triggers

### 4.1 One shared immutable-identity function

Rather than six near-identical functions, one generic function driven by
`TG_ARGV`, in the spirit of the shared `ledger_deny_mutation()` the
codebase already reuses across migrations 0022/0026/0044/0053-0063/0068/
0069/0082:

```sql
CREATE FUNCTION catalogue_enforce_immutable_identity() RETURNS TRIGGER AS $$
DECLARE
    v_col TEXT;
    v_old JSONB;
    v_new JSONB;
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION '%: % is not permitted - catalogue rows are referenced by bets, launch sessions, provider rounds and risk rules; disable (casino_games.status) or cancel (sb_events.status) the row instead (ADR 0081)', TG_TABLE_NAME, TG_OP;
    END IF;
    v_old := to_jsonb(OLD);
    v_new := to_jsonb(NEW);
    FOREACH v_col IN ARRAY TG_ARGV LOOP
        IF v_old -> v_col IS DISTINCT FROM v_new -> v_col THEN
            RAISE EXCEPTION '%.% is immutable after creation (ADR 0081) - a catalogue row''s identity may never be repointed at a different provider entity', TG_TABLE_NAME, v_col;
        END IF;
    END LOOP;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
```

The `to_jsonb` conversions are **inside the body, after the DELETE
branch**, deliberately: in a `BEFORE DELETE FOR EACH ROW` trigger `NEW`
is unassigned and touching it in a `DECLARE` initializer raises a
confusing plpgsql error before the intended message is ever reached.
Migration `0044`'s `assets_enforce_immutable_identity` has the same
structure for the same reason.

### 4.2 Exact immutable column list per table — derived from the schema

| Table | Immutable | Explicitly mutable |
|---|---|---|
| `casino_games` | `id`, `provider_id`, `provider_game_id`, `created_at` | `name`, `game_type`, `rtp_variant`, `volatility`, `feature_flags`, `supported_assets`, `mobile_supported`, `demo_supported`, `jurisdiction_blocklist`, `status` |
| `sb_sports` | `id`, `external_ref`, `created_at` | `code`, `name` |
| `sb_competitions` | `id`, `external_ref`, `sport_id`, `created_at` | `name` |
| `sb_events` | `id`, `external_ref`, `competition_id`, `created_at` | `name`, `start_time`, `status` |
| `sb_markets` | `id`, `external_ref`, `event_id`, `created_at` | `name`, `status` |
| `sb_selections` | `id`, `external_ref`, `market_id`, `created_at` | `name`, `odds_numerator`, `odds_denominator`, `status` |

Reasoning, per group:

* **`id`** — the platform mints its own identity once and never re-derives
  it from a provider reference (ADR 0025 §2; migration `0078`'s
  `external_ref` comment). Everything downstream points at it.
* **`provider_id`/`provider_game_id` and `external_ref`** — the natural
  key each upsert conflicts on, so no legitimate writer changes them.
  Freezing them prevents repointing an existing platform row at a
  *different* provider entity, which would silently reinterpret every
  historical row that references it.
* **Parent links (`sport_id`, `competition_id`, `event_id`,
  `market_id`)** — this is the one non-obvious inclusion, and it is the
  financially load-bearing one. `sportsbook_bets.selection_id` resolves
  its event through `sb_selections → sb_markets → sb_events`
  (`getSelectionWithContext`). Re-parenting a selection would silently
  turn an accepted bet into a bet on a different fixture and would feed
  `PlaceBet` the wrong event status. The current sync's
  `ON CONFLICT … DO UPDATE SET market_id = EXCLUDED.market_id` writes
  these columns, but always with the same value, so the trigger's
  `IS DISTINCT FROM` comparison never fires in normal operation. A
  genuine provider re-parent will now fail the startup sync loudly
  instead of corrupting bet resolution silently — **an accepted,
  intentional fail-closed behaviour** whose remediation is a reviewed
  platform-admin correction, not an automatic overwrite.
  `sb_competitions.sport_id` is included for the same structural reason
  one level up; its rationale is presentational-tree integrity (the
  browse tree and `GetEventDetail`'s sport/competition join), not money,
  and that weaker justification is stated rather than dressed up.
* **`created_at`** — migration `0044`'s treatment of `assets.created_at`.
* **`sb_selections.odds_*` are deliberately left mutable.** See §2.5.2.
* **`casino_games.game_type` is deliberately left mutable**, even though
  it feeds bonus wagering-contribution weighting
  (`bonus.RecordCashFundedWageringContribution`, called from
  `internal/casino/orchestrator.go:979-989`): already-recorded
  contributions are historical rows and are unaffected, and a provider
  legitimately re-categorising a title must not require a schema change.
  Its change *is* made visible — see §7.1's audit widening.

### 4.3 Deny-DELETE and deny-TRUNCATE (all six)

```sql
CREATE TRIGGER <t>_immutable_identity
    BEFORE UPDATE ON <t>
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity('<col>', '<col>', …);

CREATE TRIGGER <t>_deny_delete
    BEFORE DELETE ON <t>
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER <t>_no_truncate
    BEFORE TRUNCATE ON <t>
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
```

Exact trigger names, all twelve plus six:

`casino_games_immutable_identity`, `casino_games_deny_delete`,
`casino_games_no_truncate`, `sb_sports_immutable_identity`,
`sb_sports_deny_delete`, `sb_sports_no_truncate`,
`sb_competitions_immutable_identity`, `sb_competitions_deny_delete`,
`sb_competitions_no_truncate`, `sb_events_immutable_identity`,
`sb_events_deny_delete`, `sb_events_no_truncate`,
`sb_markets_immutable_identity`, `sb_markets_deny_delete`,
`sb_markets_no_truncate`, `sb_selections_immutable_identity`,
`sb_selections_deny_delete`, `sb_selections_no_truncate`.

TRUNCATE binds the shared `ledger_deny_mutation()` (migration `0021`),
which is already generic over `TG_TABLE_NAME`/`TG_OP` — the codebase's
established convention. A statement-level trigger is required separately
because row-level triggers never fire on TRUNCATE (ADR 0013's Stage 2
correction for `audit_log`).

**Accepted consequence, stated plainly:** deny-DELETE means the
sportsbook catalogue grows monotonically. A finished fixture is
`status = 'finished'`, never a removed row, because `sportsbook_bets`
resolves through it. If catalogue volume later becomes an operational
problem, the answer is a reviewed, dual-controlled archival operation —
not re-opening DELETE. That is a future decision, not authorized here.

---

## 5. Four-eyes on `casino_games.jurisdiction_blocklist` — ruling

**Warranted — but in the opposite direction from the one the dispatch
asked about, and only there.**

The `assets` precedent draws an explicit, deliberate asymmetry (migration
`0044` §2): dual control is required for operations that bring a gate
from **off to on** (create, activate, platform-authorize) and is
*deliberately not* required for the reverse (suspend, revoke), because
"turning something off is the fail-closed direction and an emergency
kill-switch must not need a second approver."

Applying that same rule to `casino_games`:

* **Adding** a code to `jurisdiction_blocklist` *denies* play. It is the
  fail-closed direction — a compliance kill-switch. Requiring a second
  approver would delay exactly the action that must be instant.
  **Single-actor, audited. No four-eyes.**
* **Removing** a code from `jurisdiction_blocklist` *re-permits* play in
  a jurisdiction previously recorded as a licence problem. This is the
  fail-open, compliance-widening direction, and it is the precise analog
  of `assets.platform_authorized` going false → true. **Four-eyes.**
* `status` `'active'` → `'disabled'` is fail-closed: **no four-eyes**.
  `'disabled'` → `'active'` is a platform-wide re-enablement:
  **four-eyes**, same mechanism.
* **Creation is NOT dual-controlled** — see §2.5.1. This is the ruling's
  main verified departure from the `assets` precedent.

**Not warranted anywhere in the five `sb_*` tables.** They carry no
compliance denial control (no blocklist column exists —
`internal/sportsbook/orchestrator.go:102-104`), and four-eyes on a
machine catalogue sync is a category error.

### 5.1 Phased delivery (important)

The four-eyes control requires new governance tables **and** new API
surface (a request endpoint and an approve endpoint), because the
existing `PUT /v1/admin/casino/games` submits a whole desired
`jurisdiction_blocklist` array and would simply start failing on any
removal. That is a larger change than the backstop itself.

* **Phase 1 = migration `0084`** (§3, §4). Closes `ARCH-DB-2` as written.
  No API change beyond re-scoping two call sites. Ships alone.
* **Phase 2 = migration `0084` + `internal/casino` + handlers + routes**
  (§5.2). Closes the separate single-actor compliance-control concern.
  **RECOMMENDED, specified below, and explicitly NOT authorized by this
  ADR** — it adds public API surface, so it needs orchestrator sequencing
  and its own stage slot. Until it ships, `jurisdiction_blocklist`
  remains single-actor, which is the status quo, not a regression.

### 5.2 Phase 2 specification (for when it is authorized)

Two tables, mirroring migration `0044` §2 exactly, platform-scoped (no
`tenant_id` — these govern platform-scoped operations on a platform-scoped
table; `ENABLE` + `FORCE` RLS with a single `platform_admin_scope`
`FOR ALL` policy identical to `asset_change_requests`'):

`casino_catalogue_change_requests` — `id`,
`operation TEXT NOT NULL CHECK (operation IN ('jurisdiction_unblock','status_activate'))`,
`game_id UUID NOT NULL REFERENCES casino_games (id)` (a real FK is
possible here, unlike `asset_change_requests.asset_code`, because the row
always already exists), `payload JSONB NOT NULL DEFAULT '{}'::jsonb` with
`CHECK (operation <> 'jurisdiction_unblock' OR payload ? 'removed_codes')`,
`reason_code TEXT NOT NULL`, `requested_by_principal_id UUID NOT NULL`,
`requested_at`, `state` (`pending|applied|rejected|cancelled`),
`applied_by_principal_id`, `applied_at`,
`CHECK ((state = 'applied') = (applied_at IS NOT NULL))`.

`casino_catalogue_change_approvals` — `id`, `request_id` FK,
`approver_principal_id`, `decision` (`approve|reject`), `reason_code`,
`decided_at`, `UNIQUE (request_id, approver_principal_id)`,
`CHECK (decision = 'approve' OR reason_code IS NOT NULL)`.

Triggers (names exact): `casino_catalogue_change_requests_immutable`,
`_deny_delete`, `_no_truncate`, `_platform_principal` (requester must
resolve to a `staff_users` row with `tenant_id IS NULL`);
`casino_catalogue_change_approvals_immutable`, `_no_truncate` (both
binding `ledger_deny_mutation()`), `_deny_self_approval` (blocking both
the same principal and the same `staff_users.person_id`, exactly as
migration `0044`'s `asset_change_approvals_deny_self_approval` and
migration `0029`'s withdrawal precedent do).

Function `casino_catalogue_change_consume_approved_request(p_game_id UUID,
p_operation TEXT) RETURNS UUID` — a verbatim structural copy of
`asset_change_consume_approved_request`, keyed on `game_id` instead of
`asset_code`: selects one pending request with at least one approval from
a principal distinct from the requester and no rejection, `FOR UPDATE`,
and marks it `applied` in the same statement that performs the mutation.
Consumption inside the trigger is what makes the control
non-forgettable and non-replayable.

Trigger `casino_games_dual_control` (`BEFORE UPDATE ON casino_games`,
function `casino_games_enforce_dual_control()`):

* `IF NEW.status = 'active' AND OLD.status = 'disabled'` → consume
  `'status_activate'`.
* Compute
  `v_removed := ARRAY(SELECT unnest(OLD.jurisdiction_blocklist) EXCEPT SELECT unnest(NEW.jurisdiction_blocklist))`.
  `IF array_length(v_removed, 1) IS NOT NULL` → consume
  `'jurisdiction_unblock'`, then verify the consumed request's
  `payload -> 'removed_codes'` equals `v_removed` as sorted sets, raising
  if not (approving "unblock DE" must not let "unblock DE, FR" through —
  the same payload-match discipline migration `0044` applies to an
  approved `create`).
* Additions to the blocklist with no removals, and every other column
  change, pass through untouched.

API surface Phase 2 must add (outline, exact contract is Phase 2's own
design item): `POST /v1/admin/casino/games/{gameID}/change-requests` and
`POST /v1/admin/casino/change-requests/{requestID}/approvals`, both under
a new platform-only permission `PermCasinoCatalogueGovern`, both audited,
both running under `db.Pool.WithPlatformAdmin`.

---

## 6. Migration `0084` — shape (do not write the `.sql` yet)

`migrations/0084_catalogue_write_authorization.up.sql`, in this order:

1. Header comment: names `ARCH-DB-2`, this ADR, and the `assets`
   precedent; records the §2.2 "no `tenant_id` column, deliberately"
   ruling and the §2.5 departures so the reasoning is readable at the
   schema, per this repo's migration-comment convention.
2. `CREATE FUNCTION catalogue_enforce_immutable_identity()` (§4.1).
3. For each of the six tables, in this order — `casino_games`,
   `sb_sports`, `sb_competitions`, `sb_events`, `sb_markets`,
   `sb_selections`:
   `ENABLE` + `FORCE ROW LEVEL SECURITY`; the four policies (§3.3); the
   three triggers with the exact `TG_ARGV` column list from §4.2 (§4.3).
4. No data statements. No column additions. No CHECK-constraint changes.
   Nothing here alters an existing column's type, an existing policy, or
   an existing constraint.

`migrations/0084_catalogue_write_authorization.down.sql`: drop the
eighteen triggers, drop the twenty-four policies, `DISABLE ROW LEVEL
SECURITY` on all six (`NO FORCE` first), drop
`catalogue_enforce_immutable_identity()`. The down migration is a clean
inverse; a full up → down → up round-trip must be verified, per this
repo's standing migration gate.

The S9-12 review's "migration-safety Rules A-E for 0083+" are referenced
in `docs/governance/task-registry.md` row S9-12 but their text is **not
present anywhere under `docs/`** (I searched). The implementing wave must
obtain them from the Stage 9 report before writing `0084`; if they cannot
be located, the constraints in this section stand on their own.

---

## 7. Call sites that must change

### 7.1 `internal/httpserver/casino_admin_handlers.go` — `newUpsertCasinoGameHandler`

* Line 100: replace `subjectID, _ := uuid.Parse(tc.Subject)` with an
  explicit error check returning `apierror.CodeUnauthorized` /
  `"no authenticated context"`. **Required, not cosmetic**: a swallowed
  parse failure becomes `uuid.Nil`, and `WithPlatformAdmin` rejects that
  with the opaque `db: WithPlatformAdmin called with nil principal id`.
  `admin_routes.go:73-84` documents this exact hazard and is the pattern
  to copy verbatim.
* Line 102: replace
  `deps.DB.WithoutTenant(r.Context(), func(ctx, tx) error {`
  with
  `deps.DB.WithPlatformAdmin(r.Context(), subjectID, func(ctx, tx) error {`.
  The closure body is otherwise unchanged.
* The existing `audit.Record` call needs **no** change: it already omits
  `TenantID`, and `WithPlatformAdmin` (like `WithoutTenant`) never sets
  `app.tenant_id`, so `audit_log`'s dual-scope RLS `WITH CHECK` is
  satisfied identically.
* Update the handler's doc comment (lines 39-46), which currently states
  "run under `db.Pool.WithoutTenant` … since `casino_games` (migration
  0035) carries no RLS at all". After this change that sentence is false.
* **Recommended, same wave:** widen the SEC-4I-F3 before/after audit
  metadata (lines 124-138) to include `name` and `game_type` alongside
  `status`, `supported_assets`, `demo_supported` and
  `jurisdiction_blocklist`, since §4.2 deliberately leaves `game_type`
  mutable and it feeds bonus wagering weighting.

### 7.2 `internal/casino/catalogue.go` — `UpsertGame`

Add, as the function's first statement, a Go-level scope assertion
mirroring `internal/jurisdiction`'s `assertPlatformScope`
(`evaluation_policy_admin.go:79-94`): read
`app.platform_admin_principal_id`, `app.tenant_id`,
`app.player_account_id` in one round trip and return a new sentinel
`ErrTransactionScope` (in `internal/casino/types.go`) unless the first is
non-null and the other two are null. Defence in depth, matching how
`identity.CreateTenant` and `jurisdiction.AssignTenantLicence` pair a Go
assertion with the DB policy. Update `GetGameByID`'s doc comment
(lines 55-58), which claims `casino_games` carries no RLS.

### 7.3 `cmd/platform-api/main.go` — the startup sync

Line 152: replace

```go
if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
    return sportsbook.SyncCatalogue(ctx, tx, sportsbook.NewMockSportsbookProvider())
}); err != nil {
```

with

```go
if err := pool.WithPlatformService(ctx, db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
    return sportsbook.SyncCatalogue(ctx, tx, sportsbook.NewMockSportsbookProvider())
}); err != nil {
```

and extend the surrounding comment (lines 139-151) to say why. The
existing `fmt.Errorf("sync sportsbook catalogue: %w", err)` wrap and its
fail-the-startup behaviour are correct and must be kept: if the sync
cannot authorize itself, the binary must not serve traffic with a
half-synced catalogue.

`internal/sportsbook/catalogue.go` — `SyncCatalogue` must call
`db.AssertPlatformServiceScope(ctx, tx, db.ServiceSportsbookCatalogueSync)`
as its first statement, and its doc comment (lines 20-22), which
currently instructs callers to use `WithoutTenant`, must be corrected.
Check for an import cycle first: `internal/sportsbook` does not import
`internal/db` today. If a cycle exists, inline the same three-GUC
`SELECT` locally rather than restructuring packages.

### 7.4 Test fixtures that will break (all must be re-scoped)

Raw catalogue `INSERT`s that will start failing under the new policies:

* `internal/sportsbook/orchestrator_integration_test.go:211,216,221,226,231`
* `internal/httpserver/sportsbook_flow_integration_test.go:58,63,68,73,78`
* `internal/casino/orchestrator_integration_test.go:130,464` and
  `internal/casino/jurisdiction_blocklist_integration_test.go:46`
  (`UpsertGame` under a non-platform-admin tx)
* `internal/httpserver/casino_flow_integration_test.go:79` (same)
* Also inspect, via their helpers:
  `internal/httpserver/stage6_b2c_sportsbook_acceptance_test.go`,
  `internal/httpserver/stage9_concurrency_integration_test.go`,
  `internal/bonus/wave3_phase2_migrations_integration_test.go`

Each must be wrapped in `WithPlatformService(…, ServiceSportsbookCatalogueSync, …)`
or `WithPlatformAdmin(…)` as appropriate. Re-scoping a fixture so it
passes is legitimate; loosening a policy so a fixture passes is not.

### 7.5 `internal/db/runtime_role_separation_test.go` — the assertion that inverts

`TestRuntimeRole_CasinoGamesExceptionUnchanged` (lines 262-297) currently
**asserts that `casino_games` has RLS disabled** and fails if
`relrowsecurity` or `relforcerowsecurity` is true. After migration `0084`
this test is wrong by construction. It must be rewritten — keeping its
spirit, which is good — into a test that asserts the *new* invariant for
all six tables: `relrowsecurity AND relforcerowsecurity` are both true,
no `tenant_id` column exists (that part stays, and is now an intentional
positive assertion backed by §2.2), and the expected policy names are
present. Suggested name:
`TestRuntimeRole_PlatformCatalogueTablesForceRLSAndHaveNoTenantColumn`.

### 7.6 New tests `qa` must specify (architectural invariants, not a test plan)

I am not writing the test strategy — that is `qa`'s. These are the
invariants it must cover:

1. A `WithTenant` connection cannot INSERT or UPDATE any of the six.
2. A `WithPlayerScope` connection cannot INSERT or UPDATE any of the six.
3. A `WithoutTenant` connection cannot INSERT or UPDATE any of the six
   (this is the one that proves the fix is not theatre — it is the scope
   both writers used before).
4. `WithPlatformAdmin` can write `casino_games` and **cannot** write any
   `sb_*` table.
5. `WithPlatformService(ServiceSportsbookCatalogueSync)` can write the
   five `sb_*` tables and **cannot** write `casino_games`.
6. Every scope, including unauthenticated/`WithoutTenant`, can still
   SELECT all six; the public `GET /v1/sportsbook/sports` and
   `GET /v1/sportsbook/events/{id}` routes still return data.
7. `WithPlatformService` rejects an unknown service string before opening
   a transaction.
8. Each immutable column, per §4.2's table, raises on a genuine change;
   a re-write of the same value does not.
9. DELETE and TRUNCATE are refused loudly (a named exception), not
   silently as a zero-row no-op.
10. `SyncCatalogue` is still idempotent across two consecutive runs under
    the new scope.
11. Bet placement end-to-end still works, with the frozen-odds invariant
    on `sportsbook_bets` unchanged.

---

## 8. Consequences

* `ARCH-DB-2` moves from "no database backstop exists" to "a database
  backstop exists, keyed on a named platform scope". It does **not**
  become "tenant-isolated", because there is no tenant boundary to
  isolate (§2.2) — and this ADR says so rather than manufacturing one.
* `internal/db` gains one narrowly-scoped concept (`PlatformService`,
  one member). ADR 0014 §1 is refined, not replaced.
* Two production call sites and roughly a dozen test fixtures re-scope.
  No business logic, no schema columns, no ledger behaviour changes.
* Future migrations touching these six tables must use §3.4's escape
  hatch. This is new friction and is intentional.
* The sportsbook catalogue becomes delete-free and its parent links
  become frozen; a genuine provider re-parent fails startup loudly
  (§4.2), which is the intended fail-closed trade.
* Four-eyes on `casino_games` compliance-widening is **specified but not
  authorized** (§5.1). Until Phase 2 ships, that control remains
  single-actor — unchanged from today, and labelled `NOT IMPLEMENTED`.
* `docs/security/runtime-role-separation.md`'s six-table exception list is
  updated to point here; the exception itself does not fully disappear
  until migration `0084` is actually applied (§9).

## 9. Status labels (CLAUDE.md "No fake completion")

* This ADR: **IMPLEMENTED** (as a design ruling — a document).
* Migration `0084`, `db.WithPlatformService`, the call-site re-scoping:
  **NOT IMPLEMENTED**. No `.sql` file and no Go file was created or
  modified by the pass that wrote this ADR.
* Phase 2 four-eyes (migration `0084` + governance API):
  **NOT IMPLEMENTED**, and not authorized.
* Nothing here was executed against any database, production or
  otherwise.
