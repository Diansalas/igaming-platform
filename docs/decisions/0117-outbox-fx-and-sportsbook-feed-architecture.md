# ADR 0117: Transactional Outbox / Event Bus, FX ConversionOperation, Sportsbook Feed Abstraction

**Status: PROPOSED** (architect, 2026-10-12, HEAD `489995e`). Design only; nothing is implemented,
and no stage is authorised. Each part needs its listed reviews before ACCEPTED; ledger-finance owns
every money invariant. Covers gap-audit items 12-14 (`docs/governance/platform-completion-gap-audit-2026-10-10.md`
§1 "FX"/"Event bus", §4 "Sports-data provider abstraction"/"Settlement"). MOCK means MOCK: no real
FX provider or sports-data vendor is designed, named or assumed.

Builds on, without reopening: ADR 0021 (`ConversionOperation`, DS-1/2/3), ADR 0037 Part B (FX Rate
Provider, Conversion record, fail-closed 1-9, plausibility B.7), 0082 (lock order), 0083, 0087/0088
(settlement contract), 0095 (no provider I/O inside a transaction), 0097 (webhook admission), 0102
(alerting), 0106 (KYC outbox), doc 09 §1.4/§3.5/§4.

## Part A: Transactional outbox and event bus

### A.1 Verified context

- `internal/eventbus/eventbus.go:60-90`: `InMemoryBus` is a STUB. It is synchronous, not durable,
  and has no importer outside tests (gap audit §1). `Publisher.Publish(ctx, Event)` takes no
  transaction, so it cannot be atomic with a business write.
- Existing durable "outbox-like" mechanisms. All of them stay as they are.

| Mechanism | Kind | Files | Disposition |
|---|---|---|---|
| `kyc_submission_outbox` | **Command** outbox (vendor call intent), lease/claim-token CAS, worker identity `kyc_submission_worker` | migration 0114, `internal/kyc/outbox_worker.go`, ADR 0106 | **Unchanged.** A command to one vendor with its own state machine is not a domain event. It may *additionally* emit `kyc.submission.terminal` (A.6). |
| `alerts` / `alert_occurrences` / `alert_deliveries` | Durable alert store plus append-only delivery log, dispatcher identity `alert_dispatcher` | migration 0110, `internal/alerting/dispatcher*.go`, ADR 0102 | **Unchanged.** Alerting is a *consumer* target, not something the outbox replaces. The event-consumer runtime raises alerts through `alerting.Raise`. |
| `deposit_intents` / `payment_attempts` + sweeper | Phase A/B/C provider-I/O intents (ADR 0095), not an outbox | migrations 0025/0101, `internal/payments/sweeper*.go` | **Unchanged.** |
| `bulk_grant_jobs` | Job queue | migration 0060 | **Unchanged.** |
| `bonus_ledger_sweep_watermarks` | Ledger-tailing consumer cursor `(posted_at, id)` | migration 0068 (its own ORDERING CAVEAT) | Unchanged now. It is a candidate to migrate onto A's consumer runtime later (A.9 slice A5). |
| `audit_log` | Synchronous, in-transaction, append-only system of record | migration 0014, `internal/audit` | **Never replaced by events.** CLAUDE.md requires the audit record in the same transaction. The "audit consumer" below is an *export feed* only. |

### A.2 Decision

1. **Same-transaction write.** Events go into an append-only `platform_events` table in the
   business transaction, never published after commit or to memory. New `internal/outbox`:
   `Emit(ctx, tx pgx.Tx, e outbox.Event) (EmitResult, error)`. `eventbus.Publisher` (no tx) is
   **deprecated for domain events**; `InMemoryBus` stays a test double.
2. **Fan-out at insert, state-based delivery.** An `AFTER INSERT` trigger inserts one
   `event_deliveries` row per enabled matching `event_subscriptions` row, in the same transaction.
   **There is no high-water-mark cursor**: a sequence cursor skips rows when commit order differs
   from sequence order (migration 0068's caveat), and an `xmin`-filtered cursor still misses a
   later-xid transaction holding a lower sequence value. State-based claiming cannot skip a row.
3. **The relay is a consumer runtime.** Workers run as a new platform service `event_relay`
   (`internal/db/platform_service.go`) and claim due deliveries with `FOR UPDATE SKIP LOCKED`, lease
   and claim token, as `internal/kyc/outbox_worker.go:241-259` does. **Pluggable:** an external
   broker is added later as one more consumer, `broker_sink`, behind a `Sink` interface; producers
   and schema do not change. No broker is built now.
4. **At-least-once with idempotent consumers.** A DB-local consumer applies its effect and the CAS
   `claimed→done` **in one transaction** under `WithTenant(delivery.tenant_id)`, so the effect is
   exactly-once. External sinks are at-least-once, keyed by `event_id`.
5. **Ordering per partition key, never global.** `partition_key` defaults to the aggregate id
   (`player_account_id`, `bet_id`, `conversion_id`). An *ordered* subscription never dispatches N for
   `(subscription, partition_key)` while a lower-`seq` delivery there is non-terminal or `dead`. This
   equals commit order **only when a partition's producers are serialised by an existing L1/L3 lock
   and `Emit` is the last statement before commit** (INV-OB-7). A tenant-wide total order is **not
   offered** (it would need an L0 advisory on every financial write); no A.6 consumer needs it.
6. **Tenant isolation by RLS plus a guard trigger.** **Payloads carry ids and money facts only**: no
   PII, secrets or raw provider payloads; consumers re-read under their own RLS scope.

### A.3 Data model (one migration)

```
platform_events (append-only; UPDATE/DELETE/TRUNCATE refused by trigger, except retention fn A.5)
  seq            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY
  event_id       UUID NOT NULL UNIQUE DEFAULT gen_random_uuid()
  tenant_id      UUID NULL REFERENCES tenants(id)    -- NULL only for platform-scope events (e.g. sportsbook market resulted)
  event_type     TEXT NOT NULL CHECK (event_type ~ '^[a-z_]+(\.[a-z_]+){1,3}$')
  schema_version SMALLINT NOT NULL CHECK (schema_version >= 1)
  partition_key  TEXT NOT NULL CHECK (length(partition_key) BETWEEN 1 AND 128)
  aggregate_type TEXT NOT NULL, aggregate_id UUID NOT NULL
  dedupe_key     TEXT NOT NULL CHECK (length(dedupe_key) <= 200)   -- producer-derived, e.g. 'ledger_tx:<uuid>'
  causation_id   UUID NULL, correlation_id UUID NULL
  payload        JSONB NOT NULL CHECK (pg_column_size(payload) <= 16384)
  occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now()
  xid            xid8 NOT NULL DEFAULT pg_current_xact_id()        -- forensics/lag metrics only, never a cursor
  UNIQUE INDEX (tenant_id, dedupe_key) WHERE tenant_id IS NOT NULL
  UNIQUE INDEX (dedupe_key)            WHERE tenant_id IS NULL

event_subscriptions (platform config; written only by migration or a platform-staff dual-control route)
  subscription   TEXT PRIMARY KEY            -- closed vocabulary mirrored in Go, e.g. 'aml_feed', 'bi_export'
  event_type     TEXT NOT NULL, ordered BOOLEAN NOT NULL, enabled BOOLEAN NOT NULL
  max_attempts   INT NOT NULL CHECK (max_attempts BETWEEN 1 AND 100)
  tenant_scoped  BOOLEAN NOT NULL            -- false = may receive tenant_id NULL events

event_deliveries
  id, seq (FK platform_events.seq), subscription (FK), tenant_id (forced from event), partition_key (copied)
  state          TEXT CHECK (state IN ('pending','claimed','done','dead','skipped'))
  claim_token, claimed_at, lease_expires_at, attempts, next_attempt_at, last_error_class (closed set),
  terminal_at, skip_reason_code, skipped_by_staff_id
  UNIQUE (subscription, seq)
  INDEX (subscription, partition_key, seq) WHERE state <> 'done'      -- ordered-dispatch NOT EXISTS probe
  INDEX (next_attempt_at) WHERE state = 'pending'; INDEX (lease_expires_at) WHERE state = 'claimed'
  CHECKs mirroring kyc_submission_outbox: claimed <=> lease; terminal <=> terminal_at; skipped <=> reason
```

**RLS.** For `platform_events`: INSERT `WITH CHECK (tenant_id = app.tenant_id)`. A guard trigger
*forces* `tenant_id` from the GUC and refuses any mismatch. A NULL tenant is allowed only when
`app.platform_service` names an allowed producer service (`sportsbook_feed_ingest`,
`fx_rate_ingest`). Tenant sessions have no SELECT. `event_relay` may SELECT both tables and may
UPDATE only the state columns of `event_deliveries`. A guard trigger enforces legal transitions
and the claim-token CAS, following the ADR 0102 §4.1 narrow-exception pattern.

### A.4 Emit contract (`internal/outbox`)

- `Emit` refuses outside a tenant-scoped or allowed-platform-service transaction (`txscope`), and
  refuses an `event_type`/`schema_version` not in the Go registry (closed set, typed payload struct
  and JSON-schema test each). Money is decimal-string integer minor units plus `asset_code` only.
- Idempotent: `INSERT ... ON CONFLICT DO NOTHING` on the dedupe index. A replayed operation (e.g.
  `ledger.Post` returning `AlreadyPosted`) must not emit; if it does, it deduplicates.
- **A failed `Emit` aborts the business transaction.** It is not swallowed like `RaiseGuarded`
  (ADR 0102 §7.2): a swallowed emit silently loses an AML-feed event. This makes `Emit` a liveness
  dependency of every producer. Mitigations: one plain INSERT, an insert-only trigger, no locks
  beyond the dedupe index, and a fault-injection test per producer.
- Lock order: `Emit` is a new class **L5**, after L4 and last before commit (ADR 0082 amendment;
  the orchestrator owns amendment sections).

### A.5 Delivery, poison, retention

- Exponential backoff with jitter; `attempts >= max_attempts` → `dead`, raising
  `outbox.delivery_dead` (new ADR 0102 alert kind). In an **ordered** subscription a dead delivery
  blocks only its `(subscription, partition_key)`. Lag over threshold raises `outbox.consumer_lag`.
  Error classes are closed: `transient | permanent | lease_expired | tenant_inactive`; `permanent`
  (schema-invalid payload, unknown aggregate) goes straight to `dead`.
- Recovery is an audited platform-staff action with a reason code: **requeue** (`dead→pending`) or
  **skip** (`dead→skipped`); skip needs four-eyes on subscriptions flagged `financial` (**OQ-A2**).
- Retention: a SECURITY DEFINER retention function (the only DELETE path, audited run record)
  removes `done`/`skipped` deliveries and fully terminal events older than a configured window;
  `dead`/`pending` rows are never removed. Window aligns with HQ-E1-4 (**OQ-A1**). Backfill for a
  new subscription is an explicit, audited replay over a `seq` range, never implicit.

### A.6 Planned first event types and consumers (all consumers MOCK or log sinks initially)

| Event type | Producer (emit point) | Partition | Consumers |
|---|---|---|---|
| `wallet.ledger_posted` | `ledger.Post` callers that post (not `AlreadyPosted`): deposit credit, withdrawal transitions, casino/sportsbook postings, manual adjustment, conversion | `player_account_id` or `tenant_id` for house-only | `aml_feed` (ordered), `bi_export` |
| `conversion.executed` | B.5 execute | `player_account_id` | `aml_feed`, `bi_export`, `alerts_rules` |
| `sportsbook.bet.placed` / `.settled` | `PlaceBet`, settlement functions | `bet_id` | `bi_export`, `aml_feed` |
| `sportsbook.market.resulted` / `.state_changed` (tenant NULL) | C.2 ingestion | `market_id` | `sb_settlement_dispatch` (ordered), `bi_export` |
| `audit.recorded` (ids only) | `audit.Record` | `tenant_id` | `audit_export` (BI copy, **not** the system of record) |
| `kyc.submission.terminal` | KYC worker terminal transition | `player_account_id` | `aml_feed` |

`alerts_rules` evaluates configured thresholds and calls `alerting.Raise` (rules out of scope).

### A.7 Invariants

**OB-1** an event commits iff its business transaction commits. **OB-2** one delivery per matching
enabled subscription at commit. **OB-3** `platform_events` is immutable; the only DELETE is the
retention function. **OB-4** `tenant_id` is forced from the authenticated GUC, never the payload;
consumers run under `WithTenant(delivery.tenant_id)`. **OB-5** a DB-local consumer's effect and its
`done` transition are atomic. **OB-6** an ordered subscription never runs N+1 while N (same
partition) is non-terminal or dead. **OB-7** `Emit` is last before commit and a partition's
producers share an L1/L3 lock. **OB-8** no PII or secrets in payloads (static test over registry
structs). **OB-9** no consumer is on the bet/settlement critical path, and no authoritative balance
is derived from events.

### A.8 Failure modes and tests (qa owns the gate)

Tests: rollback of a business transaction leaves no event and no delivery. Emit fault injection aborts the
business transaction (one per producer). Duplicate emit deduplicates. A cross-tenant emit is refused by RLS
and by the guard. A tenant session cannot read events. Concurrent workers never double-claim
(SKIP LOCKED plus CAS). Lease expiry is reclaimed and the stale-token CAS loses. Consumer crash after effect
before done: the transaction rolls back, so the effect is not duplicated. For ordered partitions: an N+1 that
commits first is still delivered after N. A dead delivery blocks only its partition. Requeue/skip are audited,
and skip has four-eyes. Retention never deletes pending/dead rows. A late-committing lower-`seq` event is
still delivered (the regression test for the cursor defect). The relay identity cannot UPDATE payloads.
Load test: a p99 emit overhead budget agreed with qa.

### A.9 Slices (owners)

**A1** (architect → backend): migration, `internal/outbox` Emit + registry, `event_relay`
identity, consumer runtime, metrics, two alert kinds; no producers. **A2** (ledger-finance):
`wallet.ledger_posted` at posting call sites; proves OB-1/OB-7 under L5. **A3** (backend):
`aml_feed`, `bi_export`, `audit_export` MOCK sinks. **A4** (sportsbook): `sb_settlement_dispatch`
(= C3). **A5** (later, bonus owner): evaluate moving the migration 0068 sweep onto the runtime.

**Reviews:** security (RLS, relay identity, guard transitions, PII rule), ledger-finance (OB-1/OB-7,
emit-abort rule, L5 amendment), qa, code-reviewer.

## Part B: FX / `ConversionOperation` (MOCK rate source only)

### B.1 Verified context

- No rate source, rate table or wallet-to-wallet conversion exists. Bonus conversion is *never* a
  `ConversionOperation` (`internal/ledger/ledger.go:136`, financial-transaction-flows.md row 14).
- `assets` is open, `decimal_exponent` 0..18 (migration 0003). Eligibility is
  `assetregistry.CheckEligibility(OperationConversion)` (`internal/assetregistry/types.go:103`);
  layer 8 (rate availability) belongs to the Conversion Service (ADR 0037 §A.7).
- The ledger balances **per asset per transaction** (`ledger_entries_check_balance`, migration
  0022). `ledger_entries.amount` is `NUMERIC(38,0)` but `ledger.EntryInput.Amount` is `int64`
  (LF-16b, `internal/money/money.go:70-78`).
- House accounts are unique on `(tenant_id, account_type, asset_code)` (migration 0020). No clearing
  account type exists (`OPEN DECISION`, ADR 0021, ledger-accounting-model.md §2).
- Rounding is resolved (ADR 0021): DS-1 half-up away from zero, once, one platform rule, via
  `money.RoundToMinorUnits(exact *big.Rat, exp, ruleID)`.

### B.2 Decision

1. **Four separate components (ADR 0037 B.1):** Registry (existing); `FXRateProvider` (ADR 0037
   B.2 shape, **MOCK only**, `providerkind.Synthetic` so production refuses it); Conversion Service
   (`internal/fx`); Ledger posting (`ConversionOperation`, ledger-finance-owned).
2. **Persisted rate snapshots.** Platform service `fx_rate_ingest` polls *outside any transaction*
   (ADR 0095) and writes immutable `fx_rate_snapshots`; quotes and executions reference a snapshot
   id. A live response is never the historical record (ADR 0037 B.6).
3. **Quote then execute.** A quote fixes snapshot, amounts, fee and policy version for `quote_ttl`.
   Execute re-validates and posts **exactly one** `LedgerTransaction` of new type `conversion`.
4. **Clearing: one `fx_conversion_clearing` house account per (tenant, asset)**, keeping ADR 0021's
   shape and migration 0020's uniqueness; pair attribution lives on `conversion_operations` and
   pair reconciliation is computed from it (B.7). **Deliberate deviation from "per asset pair"**:
   per-pair accounts need a `counter_asset_code` column and a changed unique index on core
   `ledger_accounts`, and add no invariant R3 does not already check (**OQ-B1**, ledger-finance).
5. **Rounding.** `destination_amount = RoundToMinorUnits(net_source × rate_applied, exp_dest,
   rounding_rule_id)`, once, from the exact NUMERIC rate as `big.Rat` (never float). **The residual
   (|r| ≤ ½ destination minor unit) is borne by the operator via clearing** (ADR 0021: "absorbed as
   part of the platform's spread") and stored per operation as an exact rational. Ties favour the
   player on the credit. Confirm or override: **OQ-B2**; an FX-only rule needs an ADR 0021
   amendment (DS-3).
6. **Width guard.** Until LF-16b widens `EntryInput.Amount`, any amount not fitting `int64` fails
   closed (`ErrAmountOutOfRange`), never truncated: at exponent 18 that is about 9.2 whole units.
7. **Same player, different assets, `player_cash` only.** Bonus, locked, held and withdrawal-hold
   balances are never converted. Same-asset transfer is not a `ConversionOperation` and is not built.
8. **Fail closed** on ADR 0037 B.6 conditions 1-9 and B.7 plausibility (stale = `now() - as_of >
   max_rate_age`; **no configured policy means stale**), plus: asset ineligible for `conversion` at
   any layer; tenant/brand not active (money-initiation gate); either wallet not `active`; pair
   kill-switched; insufficient balance under the L3 lock.

### B.3 Data model

```
fx_rate_source_bindings (platform; dual-control writes like asset registry 0044/0047)
  id, source_kind ('mock' only CHECK until an ADR adds a real one), base_asset, quote_asset, priority,
  max_rate_age, max_deviation_bp, history_lookback, issues_provider_reference BOOL, active, version

fx_rate_snapshots (platform-wide market data, no tenant_id; append-only; readable by tenant sessions)
  id, binding_id, base_asset, quote_asset, rate NUMERIC(38,18) CHECK (rate > 0), rate_precision SMALLINT,
  as_of TIMESTAMPTZ CHECK (as_of <= fetched_at), fetched_at, provider_reference TEXT NULL,
  plausibility_baseline NUMERIC(38,18) NULL, source_kind
  UNIQUE (binding_id, as_of)

conversion_pair_policies (tenant-scoped, RLS, versioned; narrow-only under platform bindings, ADR 0037 §A.5)
  id, tenant_id, source_asset, destination_asset, version, effective_from, enabled,
  min_source_amount, max_source_amount NUMERIC(38,0), fee_bp INT CHECK (fee_bp BETWEEN 0 AND 10000),
  fee_min NUMERIC(38,0), spread_bp INT CHECK (spread_bp BETWEEN 0 AND 10000), quote_ttl INTERVAL
  CHECK (quote_ttl <= interval '60 seconds'), kill_switch BOOL, created_by, approved_by (four-eyes when fee/spread changes)

conversion_quotes (tenant RLS + player_self_scope SELECT)
  id, tenant_id, player_account_id, source_wallet_id, destination_wallet_id (NULL = create on execute),
  source_asset, destination_asset, source_amount, fee_amount (source asset), net_source_amount,
  destination_amount NUMERIC(38,0), snapshot_id, rate_applied NUMERIC(38,18), spread_bp, policy_id,
  policy_version, rounding_rule_id, residual_num NUMERIC, residual_den NUMERIC,
  status ('quoted','executed','expired','rejected'), expires_at, created_at, terminal_at
  CHECK (source_asset <> destination_asset); CHECK (source_amount = fee_amount + net_source_amount)

conversion_operations (ADR 0021 shape + ADR 0037 B.5 additions; append-only)
  id, tenant_id, player_account_id, source_wallet_id, destination_wallet_id, source_asset_code,
  destination_asset_code, source_amount, destination_amount, exchange_rate, rate_source (binding id),
  rate_timestamp, fee_amount, fee_asset_code, spread, provider_reference, rounding_rule_id,
  quote_id UNIQUE, snapshot_id, idempotency_key, ledger_transaction_id UNIQUE, created_at
  UNIQUE (tenant_id, idempotency_key)
  guard trigger: both wallets' player_account_id = row's; wallet assets match codes; values equal the quote's
```

Ledger additions (ledger-finance-owned migration): `account_type` adds `fx_conversion_clearing`
and `conversion_fee_revenue` (**OQ-B3**: new revenue type vs. `house_gaming`; recommend new).
`transaction_type` adds `conversion`, which requires `reason_code IS NULL` and exactly the entry
shape below.

### B.4 Posting shape (one transaction, idempotency key `conversion:<quote_id>`)

| Asset | Entries |
|---|---|
| Source A | Dr `player_cash`(A) `source_amount`; Cr `fx_conversion_clearing`(A) `net_source_amount`; Cr `conversion_fee_revenue`(A) `fee_amount` (omitted when 0) |
| Destination B | Dr `fx_conversion_clearing`(B) `destination_amount`; Cr `player_cash`(B) `destination_amount` |

Each asset balances independently, which satisfies the migration 0022 trigger. The spread is
already inside `rate_applied = rate × (1 − spread_bp/10000)`, so spread P&L and the rounding residual
both land in the clearing position. They are never posted as a fractional leg.

### B.5 Execute transaction (lock order per ADR 0082)

In one `WithTenant` player transaction: (1) `conversion_quotes` `FOR UPDATE` (new **L1** table,
after `payment_attempts`); (2) check `status = 'quoted'`, `now() < expires_at`, quote player =
session player, policy enabled and not kill-switched; if expired, set `expired` and **commit** with
result `QUOTE_EXPIRED` (a result, not an error); (3) re-run `CheckEligibility(conversion)` for both
assets, the tenant/brand gate and the B.6.8 risk hook; (4) `wallet.GetOrCreate` destination, then
`GetOrCreateAccount`; (5) `ledger.LockProjectionsForPosting` (L3) over the full set and check
`player_cash(A) ≥ source_amount` from its result; (6) `ledger.Post` (L4); (7) INSERT
`conversion_operations`, mark the quote `executed`, `audit.Record` (actor = player, before/after,
IP); (8) `outbox.Emit(conversion.executed)` (L5). The quote is **never re-priced at execute**. A newer snapshot does not change it, and staleness was
judged at quote time with `quote_ttl ≤ max_rate_age − age_at_quote`. The operator bears rate
movement during the TTL (**OQ-B4**).

### B.6 API and screens

- **Player API** `/v1/wallet/conversions`: `POST /quotes {source_wallet_id, destination_asset,
  source_amount}` (tenant and player from the session); `POST / {quote_id}` with an
  `Idempotency-Key` header (a replay returns the original); `GET /` and `GET /{id}`. Closed
  rejection codes: `RATE_UNAVAILABLE`, `RATE_STALE`, `RATE_IMPLAUSIBLE`, `PAIR_DISABLED`,
  `ASSET_INELIGIBLE`, `QUOTE_EXPIRED`, `INSUFFICIENT_FUNDS`, `AMOUNT_OUT_OF_RANGE`, `LIMIT_EXCEEDED`.
- **B2C:** wallet-page conversion dialog (source wallet, destination asset, amount → quote showing
  rate, fee, receive amount and expiry countdown → confirm) and conversion history.
- **Back Office (tenant):** conversions list/detail (quote, snapshot, policy version, residual,
  ledger link); pair policy editor (four-eyes on fee/spread) and per-pair kill switch; FX
  reconciliation runs/mismatches (stream `fx_conversion`).
- **Platform console:** rate source bindings (dual control); snapshot viewer with staleness and
  health; plausibility-rejection log.

### B.7 Reconciliation (`internal/reconciliation`, stream `fx_conversion`, hourly with ledger drift)

- **R1/R2:** recomputing `destination_amount` and the residual from stored `net_source_amount`,
  `rate_applied`, `rounding_rule_id` gives the stored values; ledger entries equal B.4 exactly.
- **R3 (the "nets to zero" check, stated precisely):** per-asset clearing balances cannot net to zero in
  their own asset. They are the operator's open FX position. What must net to zero, **per snapshot**,
  is the valuation identity over every operation priced on that snapshot:
  `Σ(ledger Cr clearing_A × rate_applied × 10^(exp_B − exp_A)) − Σ(ledger Dr clearing_B) − Σ residual = 0`
  (exact rational, B minor units, amounts read from the ledger not the operation rows). Any
  non-zero result is a P1 mismatch.
- **R4/R5:** no orphan `conversion` transaction or operation; clearing projection vs entries is
  covered by existing drift. Sweeping or hedging clearing positions (treasury) is out of scope.

### B.8 Invariants

**FX-1** cross-asset value moves only as a `conversion` transaction with exactly one
`conversion_operations` row. **FX-2** a quote executes at most once (`UNIQUE quote_id` + ledger
key). **FX-3** same player, different assets. **FX-4** no float anywhere: NUMERIC rate, `big.Rat`,
one rounding via `money.RoundToMinorUnits`. **FX-5** every conversion is reproducible from persisted
rows (R1). **FX-6** stale, absent, implausible or unconfigured rate means refusal. **FX-7**
`MockRateSource` is refused in production (`RefuseSyntheticInProduction`).

### B.9 Failure modes and tests (ledger-finance's 12-category rule applies)

Tests. **Normal path:** fiat→fiat (2→2), fiat→crypto (2→8), crypto→stablecoin (8→6), exponent 0.
**Rounding:** an exact half rounds away from zero; the residual is bounded and recorded.
**Duplicates/concurrency:** same-key replay; same quote with a different key returns the same
operation; two concurrent executes of one quote give one posting; concurrent conversions never
overdraw. **Refusals:** quote expiry; each B.6 condition 1-9 and B.7 forced via the MOCK source
(stale, future `as_of`, zero rate, source down, disagreement); asset ineligible at platform, tenant
and brand layers; another player's or tenant's wallet (RLS); frozen wallet; tenant/brand not active;
`int64` overflow. **Partial failure:** a fault after `Post`, before the operation insert, rolls back
everything. **Reconciliation:** R1-R4 fixtures. **Audit/events:** one audit row and one event per
execution. **Lock order:** a `lockorder_*` deadlock harness case.

### B.10 Slices (owners)

**B0** (ledger-finance): answer OQ-B1..B4; migration for account and transaction types. **B1**
(backend): `internal/fx`, `FXRateProvider`, `MockRateSource`, ingest worker, snapshots,
dual-control bindings, ADR 0037 B.6/B.7 checks; no ledger dependency. **B2** (ledger-finance):
quote/execute, posting, guard triggers, L1 amendment. **B3** (backend + frontend): player API, B2C
dialog, BO screens. **B4** (ledger-finance + backend): `fx_conversion` reconciliation and BO reads.
Prerequisite for assets with exponent above about 12: LF-16b (`EntryInput.Amount` as `*big.Int`).

**Reviews:** ledger-finance (owner), security (player authz, RLS, dual control on bindings and
policies, rate-manipulation surface), architect, qa, and product-owner-proxy (screen scope).
Compliance flag: conversions can be a layering vector. `conversion.executed` feeds the AML feed,
and whether converted funds carry special withdrawal conditions is **OQ-B5**.

## Part C: Sportsbook feed abstraction (provider-neutral, MOCK feed)

### C.1 Verified context

- `sportsbook.Provider` has only `Catalogue(ctx)` (`internal/sportsbook/types.go:370-372`); `sb_*`
  tables are platform-wide (migration 0078); `sb_selections` has current odds only, no version or
  history. Placement checks odds by **exact** equality (`orchestrator.go:232`) and reads
  event/market/selection status without a row lock (`orchestrator.go:216-230`).
- Settlement (`settlement.go:327`) is driven only by the registration-gated, non-production route
  `POST /v1/admin/sportsbook/bets/{id}/simulate-settlement-event`
  (`internal/httpserver/sportsbook_settlement_handlers.go:20-27`). It re-derives amounts
  server-side, supports generations, rollback, tombstones and composed void, and
  `actor_staff_account_id` is nullable (migration 0091).
- doc 09 §4.1 (`DataFeedProvider`) and §1.4 (PriceHistory, acceptance freezing) are made concrete
  here, not redesigned.

### C.2 Decision

1. **Add a separate `FeedProvider` interface** in `internal/sportsbook/datafeed` (doc 09 §4.3).
   `Provider.Catalogue` is unchanged and remains the bootstrap snapshot. Adapter calls never run
   with a pooled transaction held (ADR 0095).
   ```go
   type FeedProvider interface {
       Capabilities(ctx context.Context) (FeedCapabilities, error) // push|poll|both, kinds offered, versioning mode, heartbeat cadence
       Poll(ctx context.Context, cursor FeedCursor, limit int) (FeedBatch, error) // pull mode; FeedBatch{Messages []FeedMessage, Next FeedCursor}
       Health(ctx context.Context) (FeedHealth, error)
   }
   // Push mode: an adapter's HandleCallback(ctx, raw []byte) ([]FeedMessage, error), behind ADR 0097
   // admission and ADR 0091-style signature verification. Both modes converge on ingest.Apply.
   type FeedMessage struct {
       FeedID, MessageID string        // MessageID unique per feed (dedupe)
       Kind      FeedKind              // fixture | event_state | market_state | price | result | heartbeat
       EntityRef ExternalRef           // provider refs, mapped to platform ids via existing ExternalRef mapping (doc 09 §2.5)
       Version   uint64                // per-entity monotonic, as declared by Capabilities.VersioningMode
       ProducedAt time.Time
       Body      FeedBody              // canonical: EventState | MarketState | Price{Num,Den} | Result{per-selection outcome} | Fixture
   }
   ```
   `VersioningMode` (`entity_sequence | timestamp_sequence`) is adapter-declared; an adapter offering
   neither is **not admissible**. **No real provider semantics are assumed.**
2. **Ingestion is idempotent, versioned and append-only.** `ingest.Apply` runs per message in a
   transaction under the new platform service `sportsbook_feed_ingest`: deduplicate on
   `(feed_id, message_id)`; map external refs (unmapped refs are recorded `unmapped`, never
   auto-created outside catalogue sync); apply a state message **iff `Version` > the stored
   `feed_version`**, else record `stale_dropped`; append history; emit platform events (L5).
3. **Results drive settlement through the outbox.** Ingesting a `result` writes an append-only
   `sb_market_results` row (a sports-data fact, not a financial event; doc 09 §1.1/§3.5) and emits
   `sportsbook.market.resulted`. The ordered consumer `sb_settlement_dispatch` (partition
   `market_id`) runs `SettleFromResult` **one bet per transaction** under `WithTenant`. It
   enumerates active tenants and, inside each tenant's scope, open or settled bets on the market.
   It reads no cross-tenant index (**OQ-C3**). `SettleFromResult` reuses `settleBet`, `voidBet` and
   `rollbackBet` unchanged (`actor_staff_account_id NULL`, driver `feed`, `result_id` recorded):
   unsettled → settle generation 1 (payout from the bet's **frozen** odds via
   `computePotentialReturn`); void/cancelled result → void (`market_cancelled`/`push`/`data_error`);
   correction (higher `result_version`, different outcome) → rollback g, settle g+1 (ADR 0088
   machinery); same outcome → recorded no-op. The simulate route stays as non-production test support. It is no longer the only driver.
4. **State messages** apply suspend/resume/close/cancel/void; an event cancel or void cascades to
   void results via the same path. Staff trading operations are a second audited *source*
   (`source = 'staff'`) of the same messages; manual results need four-eyes (doc 09 §3.5).
5. **Bet acceptance staleness rules.** These are added to `PlaceBet` before RG, without changing the
   existing order of the other checks. (a) `ExpectedOddsVersion` must equal `sel.odds_version`
   (optional during migration, falling back to today's exact-price check). (b) Price-change policy
   (tenant config, versioned): `exact` (**default**, today's behaviour) or `accept_better` (accept
   when live ≥ expected; the bet is frozen at the **live** price) (**OQ-C1**). (c) Feed health: for
   a feed-sourced market, refuse when `now() - feed.last_heartbeat_at > heartbeat_max`, or when the
   event is `live` and `now() - sel.price_updated_at > live_price_max_age`; no configured threshold
   means refuse. (d) A pre-match market past `start_time` whose event is not `live` is refused.
   (e) Event, market and selection rows are read `FOR SHARE`: a new lock class **C0** before L0
   (ADR 0082 amendment) that makes a committed suspension linearizable against placement.
   Ingestion takes only C0 `FOR UPDATE` and never a financial lock, so no cycle exists. Rejections reuse `RejectionOddsChanged` and `RejectionEventNotOpen` and add
   `RejectionFeedStale`.
6. **Watchdog.** Heartbeat older than `suspend_after` → a platform job suspends the feed's live
   markets (fail closed, audited `watchdog`); resume only on fresh state messages.
7. **Cashout: hook only, NOT IMPLEMENTED.** Reserved `CashoutPricer.Quote(ctx, betID)`; it will need
   the bet's L1 lock, current `odds_version`, rule (5) staleness and `settlement_type = cashout`
   (doc 09 §3.5, ADR 0033 §2), under its own ADR. No code or schema now.

### C.3 Data model (platform-wide unless stated)

```
sb_feed_sources: feed_id PK, kind ('mock' only CHECK until an ADR), status, versioning_mode, last_heartbeat_at,
                 last_message_at, poll_cursor, heartbeat_max, suspend_after, live_price_max_age
sb_feed_messages: (feed_id, message_id) PK, kind, entity_type, entity_id NULL, version, payload_sha256, produced_at,
                  received_at, outcome ('applied','stale_dropped','duplicate','unmapped','rejected_invalid')
sb_events / sb_markets / sb_selections: + feed_id NULL, feed_version BIGINT, state_updated_at; selections + odds_version BIGINT
                  (platform-assigned, +1 per applied price change), price_updated_at
sb_price_history (append-only): selection_id, odds_version, odds_numerator, odds_denominator, source ('feed','staff','catalogue_sync'),
                  feed_message ref, effective_at; UNIQUE (selection_id, odds_version)
sb_state_changes (append-only): entity_type, entity_id, from_status, to_status, source ('feed','staff','watchdog'), reason_code,
                  actor_staff_id NULL, feed_message ref, at
sb_market_results (append-only): id, market_id, result_version, source, selection_outcomes JSONB {selection_id: win|lose|void},
                  is_void, feed_message ref, received_at; UNIQUE (market_id, result_version)
sportsbook_bets (tenant): + accepted_odds_version BIGINT NULL
sportsbook_bet_settlements (tenant): + result_id NULL, + driver ('simulation','feed','staff') NOT NULL DEFAULT 'simulation'
tenant config: sportsbook_price_change_policies (tenant RLS, versioned): policy ('exact','accept_better'), effective_from
```

Dead-heat and partial (fractional) outcomes are **NOT IMPLEMENTED**. A result carrying one is
`rejected_invalid` and alerted, and is never approximated.

### C.4 Invariants

**SBF-1** applied state is monotone in `feed_version`. **SBF-2** a `(feed_id, message_id)` applies
at most once. **SBF-3** a feed message never posts money; only `settleBet`/`voidBet`/`rollbackBet`
post, under the bet's L1 lock (INV-LOCK-E4 and `sportsbook_settlement_sole_writer_test.go`
unchanged). **SBF-4** payout is re-derived from the bet's frozen odds; a feed value is at most a
claim. **SBF-5** a correction is rollback + re-settlement, never an edit. **SBF-6** stale feed or
unconfigured staleness means no new bet on a feed-sourced market. **SBF-7** odds history is
append-only and `accepted_odds_version` resolves to a history row. **SBF-8** settlement tenant scope
comes from the bet row under `WithTenant`, never from the feed.

### C.5 Reconciliation

`internal/reconciliation/sportsbook_settlement.go` gains: `sb_unsettled_after_result` (open bet on
a resulted market past a threshold), `sb_outcome_vs_result` (latest effective outcome differs from
the latest `result_version`), `sb_settlement_stale_result` (settlement `result_id` not the latest).
The MOCK-statement check stays; the BO read route is gap-audit item 9.

### C.6 Failure modes and tests

Tests. **Ingestion:** duplicate message is a no-op; out-of-order version dropped and counted;
unmapped ref recorded, not created; malformed body `rejected_invalid`; push and poll give identical
outcomes. **Feed health:** heartbeat loss → watchdog suspends → placement refused
(`RejectionFeedStale`); resume only on fresh state. **Suspension race:** suspension committed first
means the bet is refused; placement holding C0 FOR SHARE makes the suspension wait; deadlock harness
includes C0. **Odds:** version mismatch refused; `accept_better` matrix. **Settlement from results:**
N bets across 2 tenants each settle under their own scope, and an inactive tenant's bet is refused
with the existing code; correction → rollback + resettle with correct generations; void after
settlement → composed void; crash mid-fan-out redelivers idempotently (generation keys); a dead
delivery blocks only its market. **Security/audit:** bad-signature callback refused before ingest;
manual result without four-eyes refused; every state change audited. **Reconciliation:** fixtures
for the three new kinds. A scripted **mock feed scenario** drives a full lifecycle end to end.

### C.7 Slices (owners)

**C1** (sportsbook + architect): `datafeed` interfaces, scripted deterministic `MockFeedProvider`
(`providerkind.Synthetic`), ingest tables, `sportsbook_feed_ingest` identity, versioned apply, price
history, state changes; no settlement change. **C2** (sportsbook): placement rules 5a-e, C0
amendment, `accepted_odds_version`, price-change policy. **C3** (sportsbook + ledger-finance; needs
A1): results, `SettleFromResult`, `sb_settlement_dispatch`, `driver`/`result_id`, C.5 kinds.
**C4** (sportsbook + frontend): BO trading operations (suspend/resume, void event, four-eyes manual
result), feed health page, catalogue fetch timeout (gap-audit item 13). **C5** (separate ADR):
cashout, multiples, real feed adapter (PROVIDER DEPENDENT).

**Reviews:** architect, sportsbook, ledger-finance (C2/C3), security (feed trust boundary: a forged
result is a direct payout vector; watchdog; four-eyes), qa, code-reviewer.

## D. Sequencing and cross-part dependencies

Track A: A1 → A2 → A3. Track C: C1 (parallel with A1) → C2 → C3 (needs A1) → C4. Track B: B0
(parallel) → B1 → B2 (needs A1 for the emit point) → B3/B4. Only B2 and C3 post money; both are
ledger-finance-owned. ADR 0082 amendments: L5 (Emit), C0 (catalogue rows), `conversion_quotes` in
L1. All of this is provider-independent; live rates and real feeds stay PROVIDER DEPENDENT.

## E. Owner questions (minimal; recommendation first)

| # | Owner | Question | Recommendation |
|---|---|---|---|
| OQ-A1 | human/compliance | Event retention window | 90 days for terminal deliveries and their events, aligned with HQ-E1-4; ledger and audit stay the record |
| OQ-A2 | security | Four-eyes to skip a dead delivery of a financial subscription? | Yes |
| OQ-B1 | ledger-finance | Clearing granularity | Per (tenant, asset) as ADR 0021, pair attribution on `conversion_operations`; per-pair only if finance needs pair P&L in-ledger |
| OQ-B2 | ledger-finance | FX rounding and residual bearer | Keep DS-1 half-up; operator bears residual via clearing; an FX-only rule needs an ADR 0021 amendment (DS-3) |
| OQ-B3 | ledger-finance | Fee revenue account | New `conversion_fee_revenue`, not `house_gaming` |
| OQ-B4 | human/commercial | Operator bears rate movement in quote TTL; fee/spread defaults | TTL ≤ 15 s; fee/spread tenant config, default 0; pair disabled until a tenant enables it |
| OQ-B5 | compliance | Extra withdrawal/AML conditions on converted funds? | None now; every conversion goes to the AML feed |
| OQ-C1 | product/human | Price-change acceptance policy | `exact` default; `accept_better` tenant opt-in |
| OQ-C2 | security/ledger-finance | Hold high-payout feed settlements for manual confirmation? | Configurable payout-threshold hold, designed in C3 review; no provider behaviour assumed |
| OQ-C3 | architect (self-decided unless objected) | Tenant enumeration vs cross-tenant open-bet index for fan-out | Enumerate now; revisit above 50 tenants |
