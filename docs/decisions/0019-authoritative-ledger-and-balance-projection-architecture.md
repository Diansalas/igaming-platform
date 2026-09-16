# ADR 0019 — Authoritative Ledger and Balance Projection Architecture

Status: Accepted (Stage 3A) and `IMPLEMENTED` (Stage 3B) by migrations
`0019`–`0028` and `internal/ledger`. One item decided here is still
outstanding: the hourly reconciliation job under "Balance serving" exists
as `internal/reconciliation.RunLedgerVsProjection` but has no scheduler
invoking it (`reconciliation-model.md` status). Derived from Blueprint §4.2
and ADR 0001, formalizing the concrete schema/RLS shape ADR 0001 left at
the principle level.

## Context

ADR 0001 fixed the *principles* (append-only, double-entry, no floating
point, DB-enforced idempotency, compensating corrections). Stage 3A
requires those principles turned into a concrete object model
(`ledger-accounting-model.md`) and a specific answer to two questions ADR
0001 didn't resolve: (1) exactly how a balance is served to callers
without every read recomputing a full `SUM` over potentially millions of
rows, and (2) exactly how RLS applies across the new ledger tables given
the mixed tenant/brand/player/house scoping established in
`financial-domain-model.md`.

## Decision

### Balance serving

- The ledger (`ledger_transactions`, `ledger_entries`) is the sole
  authoritative source. No balance is ever stored as a mutable field on
  any account/wallet row.
- A `wallet_balance_projection` materialized table is maintained
  **transactionally, in the same database transaction as the
  `ledger_entries` insert that changes it** — not via an async job, a
  trigger-only mechanism with eventual consistency, or a cache. This is
  what makes it "subordinate" rather than a second source of truth: it can
  only ever be as stale as the transaction that's still in flight, never
  independently wrong.
- **Projection grain: one row per `ledger_account_id`**, not per wallet
  (`reconciliation-model.md` §3 states the same grain). A
  `(wallet_id, account_type)` grain would cover only player-owned accounts
  and leave every house-level account (`house_gaming`, `psp_clearing`,
  `psp_reserve`, `promo_liability`, `provider_payable`,
  `jackpot_contribution`, `manual_adjustment` — all of which
  `ledger-accounting-model.md` §2 requires be reconciled) with no
  projection row to reconcile against and no lockable row for the
  `SELECT ... FOR UPDATE` path in ADR 0020. Keying on `ledger_account_id`
  (which already carries `tenant_id`, `asset_code` and a nullable
  `wallet_id`) covers both cases with one mechanism; a wallet-level balance
  is then the sum of that wallet's account rows, and the `(wallet_id,
  account_type)` view remains available as an index, not as the grain.
  Note the trade-off this exposes and does **not** hide: house-level
  accounts are single rows with very high write contention (every bet in a
  tenant/asset touches `house_gaming`), so Stage 3B must either accept
  serialized posting per tenant+asset or shard those projection rows —
  a throughput design point for the `architect`, not an invariant change.
  The ledger itself is unaffected: `ledger_entries` remains append-only.
- Every hour, a reconciliation job recomputes the true `SUM` from
  `ledger_entries` and diffs it against the projection
  (`reconciliation-model.md` §2.1, §3). Non-zero drift is a P1 incident,
  per the Blueprint's own NFR target — no tolerance band.
- Redis or any other cache is never authoritative and is never read on the
  bet/settlement path (ADR 0001, restated because this ADR is the schema-
  level authority other specialists will check against during
  implementation).

### RLS and tenancy shape

- Every ledger-adjacent table (`ledger_accounts`, `ledger_transactions`,
  `ledger_entries`, `wallet_balance_projection`, `wallets`, and the
  workflow tables in `withdrawal-state-machine.md` and
  `reconciliation-model.md`) carries `tenant_id NOT NULL`, enforced by
  `FORCE ROW LEVEL SECURITY` with a policy gated on the connection-level
  `app.tenant_id` setting — the same idiom Stage 2 established for
  `player_accounts`/`sessions`/`audit_log` (`03-database-architecture.md`,
  ADR 0016).
- Player-owned account types additionally scope by player, giving a
  player-principal-scoped RLS policy
  analogous to the hardened `sessions` design (ADR 0016) — a player reads
  only ledger entries for their own wallets, never another player's, even
  within the same tenant. **This requires the scoping column to be on
  every row the policy protects**, not reachable only by joining
  `ledger_accounts`: a policy written as a subquery into another RLS-
  protected table produces a policy whose deny behaviour depends on that
  second table's policy set, which is exactly the indirection ADR 0016 had
  to unwind. As implemented (migrations `0019`/`0020`/`0022`/`0023`), the
  column the policies actually key on is **`player_account_id`**, not
  `wallet_id`: `wallets` carries it natively, and `ledger_accounts`,
  `ledger_entries` and `wallet_balance_projection` each carry it
  denormalized (`ledger_entries` and `wallet_balance_projection` carry
  `wallet_id` too, but only for indexing/joins, never as the policy
  predicate). Denormalization is populated and cross-checked by a
  `BEFORE INSERT` trigger rather than a composite FK, because a nullable
  column in a `MATCH SIMPLE` composite FK would disable the check for
  exactly the house-level rows (`ledger-accounting-model.md` §1.3). The
  player scope GUC
  is `app.player_account_id`, set only by trusted server code from the
  authenticated player principal's own resolved account — never from a path,
  query, or body parameter (same rule `Pool.WithPrincipalScope` follows in
  ADR 0016).
- House-level account types (`house_gaming`, `provider_payable`, etc.)
  have no player scope — they are readable only by platform/tenant-staff
  roles with the appropriate RBAC permission (Stage 2's existing
  permission-based RBAC, no new primitive).
- Every uniqueness constraint on these tables is **tenant-scoped** —
  `(tenant_id, …)`, never a platform-global key on a tenant-partitioned,
  RLS-protected table (rationale: `ledger-accounting-model.md` §3). This
  applies to the ledger's two idempotency keys and to the workflow tables'
  own keys (`WithdrawalRequest.idempotency_key`,
  `ConversionOperation.idempotency_key`).
- `tenant_id` is never accepted from the client on any write path in this
  model — always derived server-side from authenticated context, per the
  platform-wide rule restated in every Stage 2/3 document.
- No ledger table is platform-scoped (crossing tenant boundaries
  intentionally) — every table in this model has exactly one owning
  tenant per row. If a future cross-tenant reporting need arises, it reads
  through a reporting layer (`12-audit-reporting-architecture.md`, CDC →
  ClickHouse), not through relaxed RLS on the operational ledger tables.
  This binds `reconciliation-model.md` §6's "platform-wide drift dashboard"
  too: that dashboard is a reporting-layer read, **not** a platform-scoped
  (`tenant_id IS NULL`) connection against `reconciliation_runs` /
  `reconciliation_mismatches`. Those two tables are `tenant_id NOT NULL`
  and single-scope; ADR 0013's dual-scope pattern deliberately does **not**
  apply to them, because a dual-scope policy would make a
  `WithoutTenant` connection a legitimate cross-tenant financial read path.

### Enumerated table list (no table in this model may be omitted)

`wallets`, `ledger_accounts`, `ledger_transactions`, `ledger_entries`,
`wallet_balance_projection`, `withdrawal_requests`, `withdrawal_approvals`,
`reconciliation_runs`, `reconciliation_mismatches`, and the payment-
orchestration state tables (`deposit_intents` and `provider_capabilities`
(ADR 0022 §2 — the tenant/brand-scoped provider capability and
routing-priority rows)) each carry `tenant_id NOT NULL` with
`FORCE ROW LEVEL SECURITY`. As built, this list has three corrections from
its Stage 3A form: there is **no `withdrawal_intents` table** (withdrawal
provider state sits on `withdrawal_requests` itself —
`withdrawal-state-machine.md` §2), **no provider-health rows**
(`ProviderHealth` is an in-memory `PaymentProvider.HealthStatus()` call, not
storage — `payment-orchestration.md` §6), and **no
provider-credential-configuration table** (none was needed this stage; the
only adapter is a mock with no per-tenant credential).
`provider_capability_amount_limits` (migration `0024`) originally had no
`tenant_id` column of its own and was protected by a subquery into
`provider_capabilities` instead — a known, documented gap, not a silent
one (`financial-domain-model.md` scoping table). Stage 3C (migration
`0030`) resolved this: the table now carries its own `tenant_id NOT
NULL`, a composite `(provider_capability_id, tenant_id)` FK, and a
direct `tenant_isolation` policy, satisfying the rule above like every
other tenant-owned table (migration `0033` additionally added the
player-scope exclusion guard every other staff/system-only financial
table already carried — see ADR 0023 §2).
`provider_capabilities` is named explicitly
because ADR 0022 §3 makes provider routing configuration per-
`(tenant_id, brand_id)`: even with no secret material in the row, a missed
policy leaks which providers, limits, priorities and enabled assets another
tenant runs on — commercially sensitive configuration, and a map of which
rail to attack. A `brand_id IS NULL` row means "all brands **of that
tenant**" and never "all tenants"; there is no platform-global capability
row (ADR 0022 §2), so no `tenant_id IS NULL` dual-scope policy (ADR 0013)
applies to this table either. This list is enumerated rather than left to
"every table in this model" because the Stage 3A security review found
`WithdrawalApproval` (`withdrawal-state-machine.md` §5) and
`ReconciliationRun`/`ReconciliationMismatch` (`reconciliation-model.md` §5)
originally specified with **no `tenant_id` column at all**, while their
prose claimed tenant-scoped RLS — a policy cannot be written against a
column that does not exist, so those tables would have shipped either
unprotected or protected only by application-level `WHERE` discipline, the
precise failure mode `CLAUDE.md`'s multi-tenancy rule exists to prevent.

### Append-only enforcement (the mechanism, not just the intent)

`ledger_transactions` and `ledger_entries` get **no `UPDATE` and no
`DELETE` policy at all** (under `FORCE ROW LEVEL SECURITY`, absence of a
permissive policy for a command is a deny), **plus** a `BEFORE UPDATE OR
DELETE` row-level trigger **and** a `BEFORE TRUNCATE` statement-level
trigger that unconditionally raise — the exact pair ADR 0013 arrived at for
`audit_log`. `ledger-accounting-model.md` §6 invariant #2 originally stated
the mechanism as "no `UPDATE`/`DELETE` grants on ... for the application
role; only `INSERT`". That is **not sufficient in this codebase**: the
application's runtime role *owns* these tables (ADR 0016 "Approaches
considered" #2), and a table owner can re-`GRANT` itself any privilege, so
`REVOKE`-based protection does not bind it. Row-level triggers also never
fire on `TRUNCATE`, which is why the second trigger is not optional (ADR
0013's own Stage 2 correction). Stage 3B must implement the trigger pair,
not the `REVOKE`.

### The ADR 0016 gotcha applies here, and bites hardest on the projection

ADR 0016 established empirically that **an `UPDATE`/`DELETE` against a row
requires that row to be visible under some `SELECT` policy**, whether or not
the statement uses `RETURNING`. Two tables in this model are mutated and
therefore inherit that constraint directly:

1. **`wallet_balance_projection`** is `UPDATE`d in the same transaction as
   every `ledger_entries` insert. If it carries a player-scoped `SELECT`
   policy (per the player-scope bullet above) and the posting path runs in a
   tenant-only-scoped transaction — which every provider-callback-driven
   posting does, since a casino bet callback has no player session and thus
   no `app.player_account_id` — the projection `UPDATE` would silently
   affect **zero rows**. The entries would commit and the projection would
   not: **silent balance drift, i.e. a P1 by construction, produced by the
   RLS design itself rather than by a bug in the posting code.** Required
   design: the projection's `SELECT` policies are multiple *permissive*
   policies OR'd together (Postgres semantics, same as ADR 0016's three
   session policies) — a tenant-scope policy covering system/staff paths
   AND a player-scope policy for player-facing reads — never a single
   policy that ANDs the player scope in. Stage 3B must additionally check
   `RowsAffected()` on that `UPDATE` and fail the whole transaction loudly
   if it is zero, for the same reason ADR 0016 added that check to the
   session-replacement write.
2. **`withdrawal_requests`** is `UPDATE`d on every state transition by
   staff/system, while also being player-readable (a player lists their own
   requests). The same OR'd-permissive-policy requirement applies, and for
   the same reason: a staff transition running under tenant scope must not
   be filtered out by a player-scope `SELECT` policy. Stage 3B's optimistic-
   concurrency `UPDATE ... WHERE state = $expected` checks `RowsAffected()`
   to detect a lost race — an RLS-invisible row produces an identical zero
   result, so without this the two failure modes are indistinguishable,
   exactly the ambiguity ADR 0016 flagged.

Neither table may be given a player-scoped `WITH CHECK`/`UPDATE` policy
that would let a player principal mutate its own row: players never write
to either table directly, only through server-side handlers acting in a
system scope.

### Tests Stage 3B must include (authorization/isolation floor)

Non-negotiable, per the `security` specialist's testing responsibility:

- A request for tenant A's wallet/ledger/withdrawal data carrying tenant
  B's otherwise-valid token returns 403/404 and **never** data.
- A direct SQL `SELECT`/`UPDATE`/`DELETE` against `ledger_entries`,
  `wallet_balance_projection`, `withdrawal_requests`, and
  `withdrawal_approvals` under tenant B's `app.tenant_id` returns/affects
  zero of tenant A's rows (proving the database, not the handler, denies).
- A player-scoped connection cannot read another player's wallet rows
  *within the same tenant* (cross-principal, not just cross-tenant — the
  gap ADR 0016 found late for `sessions`).
- A tenant-only-scoped posting transaction successfully updates
  `wallet_balance_projection` (the gotcha above, proven rather than
  assumed), and a projection `UPDATE` that affects zero rows aborts the
  transaction.
- `UPDATE`, `DELETE`, and `TRUNCATE` against `ledger_entries` /
  `ledger_transactions` all raise, executed **as the application's own
  runtime role**, not as a lesser role.
- A player principal cannot cause a `LedgerTransaction` of a
  staff/system-only `transaction_type` (`manual_adjustment`, any direct
  `house_gaming` credit, `bonus_grant`) — see the actor matrix below.
- A **signature-verified callback from provider X** that (a) names a
  `transaction_type` outside X's declared `ProviderCapability`, (b)
  references an intent/`provider_tx_id` belonging to provider Y, (c) is a
  `crypto_payment` provider emitting a custodian event, or (d) is a payments
  credential emitting a gaming posting, is rejected and posts nothing —
  proving the per-provider scoping in the actor matrix, not just the
  per-actor-class one.
- A `provider_capabilities` row for tenant A is invisible and unwritable
  under tenant B's `app.tenant_id`, and no connection scope (including a
  `WithoutTenant`/platform connection) returns capability rows across
  tenants.

### Who may originate which posting (privilege-escalation boundary)

The flows in `financial-transaction-flows.md` each name an initiating
event, but Stage 3A originally contained no single statement of which
*actor class* may cause which `transaction_type`. Without it, nothing in
the design prevents a player-facing endpoint from being wired to a posting
API that accepts an arbitrary `transaction_type`. The binding rule:

| Actor class | May originate |
|---|---|
| Player session (brand frontend) | Only `deposit` *initiation* (no posting — the posting is the PSP callback), `withdrawal_requested` (hold), and withdrawal cancellation. A player action never directly produces a credit to `player_cash`/`player_bonus`. |
| Verified provider callback (signature-verified adapter) | `deposit`, casino/sportsbook bet/win/settlement/void/rollback, `psp_*`, custodian events — scoped to the tenant resolved from the *credential the callback authenticated with*, never from a tenant/player identifier in the payload, **and further scoped to the originating provider** (see below). |
| Internal service (bonus engine, settlement job, gamification, reward orchestrator) | `bonus_grant`, `bonus_conversion`, `bonus_forfeiture`, `bonus_reversal` (added Stage 4H-A by `docs/decisions/0032-bonus-accounting.md` §7/§8 — a compensating transaction with `reverses_transaction_id` set, or a tombstone for a never-seen grant; `NOT IMPLEMENTED`, the type does not exist in the `transaction_type` `CHECK` constraint yet), `provider_settlement` — under a service identity (ADR 0014), not a player or staff identity. Gamification and the Reward Orchestrator inherit this row unchanged; they are internal services, **not** a new actor class. |
| Staff principal with explicit RBAC permission | `manual_adjustment` only, four-eyes-gated above the configured threshold, `reason_code` mandatory. |

Enforcement is server-side at the posting API boundary (a per-
`transaction_type` permitted-originator check), not in the UI and not
inferred from which HTTP handler happened to call it. `OPEN DECISION`: the
exact permission names for the staff row are Stage 3B RBAC detail; the
*existence* of the check is not optional.

**"Verified provider callback" is not one undifferentiated actor class**
(added in the ADR 0022 multi-provider review). With a single mock PSP the
row above was harmless; once the platform runs N replaceable providers
(ADR 0022) plus a custodian, reading the row as "any signature-verified
provider credential may originate any of these types" grants every adapter
the *union* of all provider-originable postings. A low-trust local-method
crypto/fiat gateway's webhook key would then be able to post a game
settlement, a `psp_*` reversal, or a custodian withdrawal-completion event.
The binding refinement, enforced at the posting API boundary:

1. A callback authenticated with provider X's credential may originate only
   the `transaction_type`s consistent with **X's own declared
   `ProviderCapability`** (ADR 0022 §2 — `provider_kind`,
   `supports_deposit`/`supports_withdrawal`/`supports_refund_reversal`,
   supported assets) for the tenant/brand that credential is bound to.
2. The posting must reference an intent/transaction the platform itself
   created **for that same `provider_id`**. Provider X can never settle,
   reverse, or complete a transaction belonging to provider Y, and
   `(provider_id, provider_tx_id)` idempotency is keyed on the provider
   resolved from the credential, never on a `provider_id` field in the
   payload.
3. A `provider_kind = 'crypto_payment'` adapter may **never** originate a
   custodian event, and no `PaymentProvider` of any kind may originate a
   posting attributed to the `CryptoCustodyProvider` leg
   (`crypto-custody-boundary.md` §1.1, ADR 0022 §4). The reverse also
   holds: a custodian credential does not gain PSP-callback privileges.
4. Game-provider callbacks (casino/sportsbook bet/win/settlement) and
   payment-provider callbacks are disjoint originator sets; a payments
   credential never originates gaming postings and vice versa.

## Consequences

- Stage 3B's migrations must include `FORCE ROW LEVEL SECURITY`, the
  append-only trigger pair, and the `app.tenant_id`/player-scope policies
  from day one — this is not a
  hardening pass applied after the fact (unlike the pre-Stage-3 sessions
  RLS debt this repository already paid down once; that mistake is not
  repeated here).
- The `wallet_balance_projection` table's write path (same-transaction
  update) is a hard requirement on every code path that inserts
  `ledger_entries` — a future contributor adding a new posting path that
  forgets to update the projection in the same transaction produces silent
  drift, so this must also be a `code-reviewer`/`ledger-finance` checklist
  item at implementation time, not just a one-time design note.
- Balance rebuild from zero (disaster recovery) is possible at any time by
  dropping and recomputing `wallet_balance_projection` from
  `ledger_entries` alone — this must be an exercised runbook, not merely a
  theoretical property, before Stage 3B is considered done.

## Review status

Design-level security review performed at Stage 3A (documentation only; no
code exists to review). **In scope of that review**: tenant/player RLS
shape, append-only enforcement mechanism, the ADR 0016 `UPDATE`-visibility
interaction, client-controllable scoping inputs, four-eyes bypass paths in
`withdrawal-state-machine.md`, and secrets handling in
`payment-orchestration.md` §10. **Not in scope / not yet verified**: any
implementation, any migration SQL, penetration testing, and the
correctness of policies that do not exist yet. Nothing here may be read as
"the financial subsystem is secure" — only as "this design, as documented,
has no *known* isolation or escalation gap as of Stage 3A". Stage 3B
requires its own review against the actual migrations and code.

**Second pass (ADR 0022, multi-provider).** A follow-up `security` review
covered the payment-provider-agnosticism addendum: the Crypto Payment
Provider vs. Crypto Custodian separation, `ProviderCapability` credential
and RLS handling, and this ADR's actor matrix under N providers. It added
the per-provider scoping of the "verified provider callback" actor class
above, `provider_capabilities` to the enumerated table list, and the
inbound-key-material / shared-credential rules in ADR 0022 §4.1–§4.2.
**Not in scope of that pass**: vendor due diligence, whether any specific
provider's API in fact returns key material, custodian contract terms, and
— as before — any code or migration, none of which exists.

## Owner

`ledger-finance`, RLS design co-owned with `security`.
