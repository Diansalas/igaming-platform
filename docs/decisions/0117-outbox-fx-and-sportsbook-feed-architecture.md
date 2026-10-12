# ADR 0117: Transactional Outbox / Event Bus, FX ConversionOperation, Sportsbook Feed Abstraction

**Status: ACCEPTED pending implementation** (architect, 2026-10-12; amended the same day with the
security and ledger-finance design reviews, both APPROVE WITH CONDITIONS; see §F). Nothing is
implemented and no stage is authorised. **Part B may not start before its review blockers (S-B1,
S-B2, LF-B-i, LF-B-ii) are designed into B0/B2 exactly as written here.** B2 and C3 post money and
are **ledger-finance-owned**. Owner questions (§E) carry orchestrator defaults taken from the
reviewers' rulings; each is reversible by its named owner. Covers gap-audit items 12-14
(`docs/governance/platform-completion-gap-audit-2026-10-10.md` §1 "FX"/"Event bus", §4
"Sports-data provider abstraction"/"Settlement"). MOCK means MOCK: no real FX provider or
sports-data vendor is designed, named or assumed.

Builds on, without reopening: ADR 0021 (`ConversionOperation`, DS-1/2/3), ADR 0037 Part B (FX Rate
Provider, Conversion record, fail-closed 1-9, plausibility B.7), 0082 (lock order, R8), 0083,
0087/0088 (settlement contract; void is terminal), 0095 (no provider I/O inside a transaction), 0097
(webhook admission), 0102 (alerting), 0106 (KYC outbox), 0110 (signed actor proof; T5/T10
residual classes), 0111 (T10 seal pattern), 0113 (governed manual adjustments), 0116 (AML
monitorable feed), doc 09 §1.4/§3.5/§4.

## Part A: Transactional outbox and event bus

### A.1 Verified context

- `internal/eventbus/eventbus.go:60-90`: `InMemoryBus` is a STUB. It is synchronous, not durable,
  and has no importer outside tests. `Publisher.Publish(ctx, Event)` takes no transaction, so it
  cannot be atomic with a business write.
- Existing durable "outbox-like" mechanisms all stay as they are:

| Mechanism | Kind | Files | Disposition |
|---|---|---|---|
| `kyc_submission_outbox` | **Command** outbox (vendor call intent), lease/claim-token CAS, identity `kyc_submission_worker` | migration 0114, `internal/kyc/outbox_worker.go`, ADR 0106 | **Unchanged.** May *additionally* emit `kyc.submission.terminal`. Its fence pattern is the template for A.3. |
| `alerts` / `alert_occurrences` / `alert_deliveries` | Durable alert store + append-only delivery log, identity `alert_dispatcher` | migration 0110, `internal/alerting/dispatcher*.go`, ADR 0102 | **Unchanged.** A consumer *target* via `alerting.Raise`. |
| `deposit_intents` / `payment_attempts` + sweeper | Phase A/B/C provider-I/O intents (ADR 0095) | migrations 0025/0101, `internal/payments/sweeper*.go` | **Unchanged.** |
| `bulk_grant_jobs` | Job queue | migration 0060 | **Unchanged.** |
| `bonus_ledger_sweep_watermarks` | Ledger-tailing cursor `(posted_at, id)` | migration 0068 (ORDERING CAVEAT) | Unchanged; candidate for slice A5. |
| ADR 0116 AML DB feed (`aml_monitorable_tx_type`) | Authoritative AML monitoring source | ADR 0116 | **Authoritative.** Outbox AML events are supplementary. |
| `audit_log` | Synchronous, in-transaction system of record | migration 0014, `internal/audit` | **Never replaced by events.** `audit_export` is an export feed only. |

### A.2 Decision

1. **Same-transaction write.** Events go into an append-only `platform_events` table inside the
   business transaction, never published after commit or to memory. New `internal/outbox`:
   `Emit(ctx, tx pgx.Tx, e outbox.Event) (EmitResult, error)`. `eventbus.Publisher` (no tx) is
   **deprecated for domain events**; `InMemoryBus` stays a test double.
2. **Fan-out at insert, state-based delivery.** An `AFTER INSERT` trigger inserts one
   `event_deliveries` row per enabled matching `event_subscriptions` row, in the same transaction.
   There is **no high-water-mark cursor**: a sequence cursor skips rows when commit order differs
   from sequence order (migration 0068's caveat), and an `xmin`-filtered cursor still misses a
   later-xid transaction holding a lower sequence value. State-based claiming cannot skip a row.
3. **Relay identity is claim-only (security condition, migration 0114 pattern).** Workers run as
   platform service `event_relay` **only** for the claim `UPDATE ... RETURNING` (`pending|expired
   claimed` → `claimed`, `FOR UPDATE SKIP LOCKED`, lease, fresh claim token). Every later
   transition of a **tenant** delivery (`claimed→done|pending|dead`) runs under
   `WithTenant(delivery.tenant_id)` through a tenant UPDATE policy limited to the state columns plus
   the claim-token CAS. **NULL-tenant** deliveries (platform-scope events) have their own terminal
   path under `event_relay`, with the same column limits and CAS. The relay identity can **never**
   write `skipped`, `dead→pending`, `skip_reason_code` or `skipped_by_staff_id` (A.5).
   **Pluggable:** an external broker is added later as one more consumer, `broker_sink`, behind a
   `Sink` interface; producers and schema do not change. No broker is built now.
4. **At-least-once with idempotent consumers.** A DB-local consumer applies its effect and the CAS
   `claimed→done` **in one transaction** under `WithTenant`, so the effect is exactly-once.
   External sinks are at-least-once, keyed by `event_id`.
5. **Events are hints, not facts (security condition).** No consumer acts on payload values. It
   re-reads the fact row (ledger transaction, conversion operation, bet, market result) under its
   own RLS scope and acts on that. A forged or stale event whose fact row is missing or unchanged is
   a **recorded no-op**.
6. **Ordering per partition key, never global.** `partition_key` defaults to the aggregate id. An
   *ordered* subscription never dispatches N for `(subscription, partition_key)` while a lower-`seq`
   delivery there is non-terminal or `dead`. That equals commit order **only when `Emit` runs after
   the partition's serialising lock is held** (OB-7 as restated by ledger-finance; deferred triggers
   such as those of 0116/0129 run later and do not affect this). `wallet.ledger_posted` is
   partitioned by **`wallet_id`**, and ordered consumers rely only on the per-wallet `player_cash`
   L3 serialisation. A tenant-wide total order is **not offered**: it would need an L0 advisory on
   every financial write, and no A.6 consumer needs it.
7. **Tenant isolation by RLS, a guard trigger and NULL-arm fences.** Payloads carry only UUIDs,
   integers, decimal strings and closed enums. No PII, secrets, free text or raw provider payloads.

### A.3 Data model (one migration)

```
platform_events (append-only; UPDATE/DELETE/TRUNCATE refused by trigger, except retention fn A.5)
  seq            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY
  event_id       UUID NOT NULL UNIQUE DEFAULT gen_random_uuid()
  tenant_id      UUID NULL REFERENCES tenants(id)    -- NULL only for platform-scope events (market resulted/state)
  event_type     TEXT NOT NULL CHECK (event_type ~ '^[a-z_]+(\.[a-z_]+){1,3}$')
  schema_version SMALLINT NOT NULL CHECK (schema_version >= 1)
  partition_key  TEXT NOT NULL CHECK (length(partition_key) BETWEEN 1 AND 128)
  aggregate_type TEXT NOT NULL, aggregate_id UUID NOT NULL
  dedupe_key     TEXT NOT NULL CHECK (length(dedupe_key) <= 200)   -- producer-derived, e.g. 'ledger_tx:<uuid>'
  payload_sha256 BYTEA NOT NULL                                    -- canonical-JSON hash; dedupe conflict comparator
  causation_id   UUID NULL, correlation_id UUID NULL
  payload        JSONB NOT NULL CHECK (pg_column_size(payload) <= 16384)
  occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now()
  xid            xid8 NOT NULL DEFAULT pg_current_xact_id()        -- forensics/lag metrics only, never a cursor
  UNIQUE INDEX (tenant_id, dedupe_key) WHERE tenant_id IS NOT NULL
  UNIQUE INDEX (dedupe_key)            WHERE tenant_id IS NULL

event_subscriptions (platform config)
  subscription   TEXT PRIMARY KEY            -- closed vocabulary mirrored in Go
  event_type     TEXT NOT NULL, ordered BOOLEAN NOT NULL, enabled BOOLEAN NOT NULL
  class          TEXT NOT NULL CHECK (class IN ('operational','financial','compliance'))   -- aml_feed = compliance
  max_attempts   INT NOT NULL CHECK (max_attempts BETWEEN 1 AND 100)
  tenant_scoped  BOOLEAN NOT NULL            -- false = may receive tenant_id NULL events
  -- writes: migration, or the actor-proof operation event_subscription:change (A.5)

event_deliveries
  id, seq (FK platform_events.seq), subscription (FK), tenant_id (forced from event), partition_key (copied)
  state          TEXT CHECK (state IN ('pending','claimed','done','dead','skipped'))
  claim_token, claimed_at, lease_expires_at, attempts, next_attempt_at, last_error_class (closed set),
  terminal_at, skip_reason_code, skipped_by_staff_id, actor_proof_id
  UNIQUE (subscription, seq)
  INDEX (subscription, partition_key, seq) WHERE state <> 'done'      -- ordered-dispatch NOT EXISTS probe
  INDEX (next_attempt_at) WHERE state = 'pending'; INDEX (lease_expires_at) WHERE state = 'claimed'
  CHECKs mirroring kyc_submission_outbox: claimed <=> lease; terminal <=> terminal_at; skipped <=> reason + staff + proof

platform_event_retention (owner-owned single row): window INTERVAL CHECK (window >= interval '30 days')
```

**RLS and fences.**
- `platform_events` INSERT is `WITH CHECK (tenant_id = app.tenant_id)`. A guard trigger *forces*
  `tenant_id` from the GUC and refuses a mismatch.
- A NULL tenant is allowed only when `app.platform_service` is in the producer allowlist constant
  (`sportsbook_feed_ingest`, `fx_rate_ingest`).
- Tenant sessions have no SELECT on events.
- **NULL-ARM-WRITE-1:** every table with a NULL-tenant arm (`platform_events`, `event_deliveries`,
  `fx_rate_snapshots`, `sb_market_results`, `sb_feed_messages`, `sb_state_changes`,
  `sb_price_history`) gets the migration 0114-style fence family. A NULL-tenant write is possible
  only under its named platform-service GUC, and each service has a single-call-site static test.
- `event_relay` may SELECT both tables. It may UPDATE `event_deliveries` only for the claim, and for
  the NULL-tenant terminal path. The guard trigger enforces legal transitions, the claim-token CAS,
  and the A.2.3 column prohibitions.

### A.4 Emit contract (`internal/outbox`)

- **Scope check.** `Emit` refuses outside a tenant-scoped or allowlisted platform-service
  transaction (`txscope`).
- **Type check.** It refuses an `event_type`/`schema_version` not in the Go registry. The registry is
  a closed set: each type has a typed payload struct and a JSON-schema test.
- **Payload check.** Registry fields may only be UUIDs, integers, decimal strings or closed enums. A
  static test refuses any free-text field. Money is a decimal-string integer of minor units plus
  `asset_code`.
- **Idempotency.** `INSERT ... ON CONFLICT DO NOTHING` on the dedupe index. On conflict, `Emit`
  compares `payload_sha256` with the stored row: equal is a no-op, while a **mismatch aborts the
  business transaction loudly** with an error log and `outbox.dedupe_mismatch`. A replayed
  operation (for example `ledger.Post` returning `AlreadyPosted`) should not emit at all.
- **A failed `Emit` aborts the business transaction.** It is never swallowed the way `RaiseGuarded`
  is. Mitigations: one plain INSERT, an insert-only trigger, no locks beyond the dedupe index, and a
  fault-injection test per producer.
- **Lock class L5** (ADR 0082 amendment). `Emit` runs after the partition's serialising lock and
  after L4. It is normally the last statement before commit.

### A.5 Delivery, poison, operator actions, retention

- **Backoff and dead deliveries.** Backoff is exponential with jitter. `attempts >= max_attempts`
  moves a delivery to `dead` and raises `outbox.delivery_dead` (a new ADR 0102 kind). In an
  **ordered** subscription, a dead delivery blocks only its own `(subscription, partition_key)`.
  Lag over threshold raises `outbox.consumer_lag`.
- **Error classes** are closed: `transient | permanent | lease_expired | tenant_inactive`. A
  `permanent` error goes straight to `dead`.
- **Operator actions are ADR 0110 actor-proof-bound operations** (security B3, OQ-A2 = yes):
  - `event_delivery:requeue` (`dead→pending`) and `event_delivery:skip` (`dead→skipped`, reason
    code required) apply to any delivery.
  - `event_subscription:change` covers enable, disable and backfill (an explicit `seq` range;
    backfill is never implicit).
  - **Skip or disable on a `financial` or `compliance` subscription (including `aml_feed`) needs
    four-eyes.**
  - Every action is audited. The relay identity can never perform any of them.
- **Retention.**
  - A retention function deletes `done` and `skipped` deliveries, plus events whose deliveries are
    all terminal, once they are older than `platform_event_retention.window`. `dead` and `pending`
    rows are never deleted.
  - The function is owned by the migration owner and is **not executable by `igaming_runtime`**.
    It writes an audited run record.
  - The window is a DB-held value with a 30-day floor. The default is 90 days (OQ-A1).

### A.6 Planned first event types and consumers (all consumers MOCK or log sinks initially)

| Event type | Producer (emit point) | Partition | Consumers |
|---|---|---|---|
| `wallet.ledger_posted` | Posting call sites that post (not `AlreadyPosted`) | `wallet_id` (house-only: `tenant_id`, unordered) | `aml_feed` (ordered, compliance), `bi_export` |
| `conversion.executed` | B.5 step 9 | `wallet_id` of the source wallet | `aml_feed`, `bi_export`, `alerts_rules` |
| `sportsbook.bet.placed` / `.settled` | `PlaceBet`, settlement functions | `bet_id` | `bi_export`, `aml_feed` |
| `sportsbook.market.resulted` / `.state_changed` (tenant NULL) | C.2 ingestion | `market_id` | `sb_settlement_dispatch` (ordered, financial), `bi_export` |
| `audit.recorded` (ids only) | `audit.Record` | `tenant_id` | `audit_export` (BI copy, not the system of record) |
| `kyc.submission.terminal` | KYC worker terminal transition | `player_account_id` | `aml_feed` |

`alerts_rules` evaluates configured thresholds and calls `alerting.Raise`; the rules themselves are
out of scope. Every consumer re-reads its fact row (A.2.5).

### A.7 Invariants

- **OB-1:** an event commits if and only if its business transaction commits.
- **OB-2:** at commit, there is one delivery per matching enabled subscription.
- **OB-3:** `platform_events` is immutable. The only DELETE is the owner-owned retention function.
- **OB-4:** `tenant_id` is forced from the authenticated GUC, never taken from the payload.
  Tenant-delivery transitions after the claim run under `WithTenant`.
- **OB-5:** a DB-local consumer's effect and its `done` transition are atomic.
- **OB-6:** an ordered subscription never runs N+1 while N (same partition) is non-terminal or dead.
- **OB-7:** `Emit` runs after the partition's serialising lock.
- **OB-8:** payload fields are only UUIDs, integers, decimal strings or closed enums (static test).
- **OB-9:** no consumer acts on payload values. Consumers re-read facts, and a missing or unchanged
  fact is a recorded no-op.
- **OB-10:** the relay never writes `skipped`, `dead→pending` or skip metadata.
- **OB-11:** no consumer is on the bet or settlement *placement* path, and no authoritative balance
  is derived from events.

### A.8 Failure modes and tests (qa owns the gate)

Tests:
- **Transaction atomicity.** A rolled-back business transaction leaves no event and no delivery.
  Emit fault injection aborts the business transaction (one test per producer).
- **Dedupe.** A duplicate emit with an equal hash is a no-op. With a different hash it aborts and
  raises an alert.
- **Tenant isolation.** A cross-tenant emit is refused by RLS and by the guard. A NULL-tenant emit
  from a non-allowlisted service is refused (NULL-ARM-WRITE-1). A tenant session cannot read events.
- **Claiming.** Concurrent workers never double-claim. An expired lease is reclaimed, and a stale
  claim token loses the CAS. The relay identity cannot complete a tenant delivery, cannot write
  `skipped`/`dead→pending`, and cannot UPDATE payloads.
- **Consumer behaviour.** A consumer crash after its effect but before `done` rolls the transaction
  back. A forged event with a missing or unchanged fact row is a no-op.
- **Ordering.** For ordered partitions, an N+1 that commits first is still delivered after N. A dead
  delivery blocks only its partition. A late-committing lower-`seq` event is still delivered.
- **Operator actions.** Requeue, skip, enable, disable and backfill without an actor proof are
  refused. Skip or disable of `aml_feed` without four-eyes is refused.
- **Retention.** `igaming_runtime` cannot execute retention. A window below 30 days is refused.
- **Load.** p99 emit overhead must stay within a budget agreed with qa.

### A.9 Slices (owners)

- **A1** (architect → backend): the migration (including fences and the retention row),
  `internal/outbox` Emit and registry, `event_relay` claim-only identity, the consumer runtime, the
  actor-proof operator actions, metrics and alert kinds. No producers.
- **A2** (ledger-finance): `wallet.ledger_posted` at the posting call sites (partitioned by
  `wallet_id`). Proves OB-1 and OB-7 under L5.
- **A3** (backend): `aml_feed`, `bi_export` and `audit_export` MOCK sinks, all re-reading facts.
- **A4** (sportsbook): `sb_settlement_dispatch`; this is the same work as C3.
- **A5** (later, bonus owner): evaluate moving the migration 0068 sweep onto this runtime.

**Reviews:**
- security: done with conditions, applied here; re-review at A1 implementation.
- ledger-finance: done with conditions, applied here.
- qa and code-reviewer: at implementation.

## Part B: FX / `ConversionOperation` (MOCK rate source only)

### B.1 Verified context

- **What exists today.** No rate source, rate table or wallet-to-wallet conversion exists. Bonus
  conversion is *never* a `ConversionOperation` (`internal/ledger/ledger.go:136`).
- **Asset registry.** `assets` is open, with `decimal_exponent` 0..18 (migration 0003).
  Eligibility is decided by `assetregistry.CheckEligibility(OperationConversion)`
  (`internal/assetregistry/types.go:103`). Layer 8 (whether a rate is available) belongs to the
  Conversion Service (ADR 0037 §A.7).
- **Ledger balancing and amount width.** The ledger balances **per asset per transaction**
  (migration 0022). `ledger.EntryInput.Amount` is `int64` (LF-16b,
  `internal/money/money.go:70-78`).
- **Rounding helper.** `money.RoundToMinorUnits(exact, exp, ruleID)` takes an exact value in
  **whole units** and scales it by `10^exp` itself.

### B.2 Decision

1. **Four separate components (ADR 0037 B.1):**
   - Registry: existing.
   - `FXRateProvider`: **MOCK only**, marked `providerkind.Synthetic`.
   - Conversion Service: `internal/fx`.
   - Ledger posting: ledger-finance-owned.
2. **Persisted, sealed rate snapshots.** `fx_rate_ingest` polls the rate source *outside any
   transaction* (ADR 0095) and writes immutable `fx_rate_snapshots`. Each snapshot carries a Go
   **ingest seal** (S-B2, below).
3. **Quote then execute.** A quote fixes the snapshot, amounts, fee and policy version. Execute
   re-validates and posts **exactly one** `LedgerTransaction` of the new type `conversion`.
   **A conversion is FINAL.** No `conversion` posting carries `reverses_transaction_id`. An error
   is corrected only by an ADR 0113 governed manual adjustment per asset (reason code, four-eyes) or
   by a new conversion at a fresh quote.
4. **Clearing account (OQ-B1).** There is one `fx_conversion_clearing` house account per
   (tenant, asset), following ADR 0021. Pair attribution lives on `conversion_operations`. Any
   future treasury sweep uses its own transaction type, never `manual_adjustment`.
5. **Exact rate, never a scaled NUMERIC (LF-B-i).** `rate_applied` is computed in Go as an exact
   `big.Rat`:
   `snapshot.rate^(+1 for direction=direct | -1 for inverse) × (10000 − spread_bp) / 10000`.
   It is persisted as `rate_applied_num` / `rate_applied_den` `NUMERIC(38,0)`, together with
   `snapshot_id`, `spread_bp` and `direction`. A guard trigger checks that `num/den` equals that
   expression **exactly**. No scaled NUMERIC rate is ever an input to a posting. (A
   `NUMERIC(38,18)` cannot hold the spread product, which needs up to 22 decimals, or an inverted
   rate.)
6. **Units and rounding (LF-B-ii, OQ-B2).** The steps are:
   - `exact = (net_source_amount / 10^exp_A) × rate_applied` (`big.Rat`, whole units of B).
   - `destination_amount = RoundToMinorUnits(exact, exp_B, rounding_rule_id)`, using DS-1 half-up
     with ties away from zero, applied once.
   - `residual = exact × 10^exp_B − destination_amount`, a **signed** exact rational stored as
     `residual_num`/`residual_den`. The operator bears it through clearing.
   - `fee_amount` is computed with the same `rounding_rule_id`.
   - `CHECK (net_source_amount > 0)`.
7. **Width guard.** Any amount that does not fit `int64` fails closed. At exponent 18 the cap is
   about 9.2 whole units, so **ETH-class (exponent 18) assets stay DISABLED for conversion until
   LF-16b is resolved.**
8. **Same player, different assets, `player_cash` only.**
9. **Wallet restrictions (fail closed with `ASSET_INELIGIBLE` or the new `WALLET_RESTRICTED`).** A
   conversion **out of** a wallet is refused when the wallet has:
   - any non-terminal bonus grant,
   - a pending withdrawal,
   - frozen or closing status, or
   - an RG self-exclusion or cool-off on the person.
10. **Other fail-closed conditions.** All ADR 0037 B.6 conditions 1-9 and B.7 apply. Absent
    configuration means stale. Also refused:
    - an asset not eligible for `conversion` at any layer;
    - a tenant or brand that is not active;
    - a tenant pair that is disabled or kill-switched;
    - a **platform binding with `active = false`**, which is the platform kill switch and overrides
      tenant policy;
    - a player account that is not active;
    - `max_source_amount` IS NULL (refused);
    - insufficient balance under L3.
11. **No reuse for reporting.** Conversion snapshots are never reused as reporting rates.
    Conversion fee and spread are **excluded from GGR**.

#### S-B1 (security blocker): quote forgery is free money, so the DB recomputes every quote

A `BEFORE INSERT` guard trigger on `conversion_quotes`:
- forces `created_at = now()`, `status = 'quoted'` and
  `expires_at = LEAST(now() + policy.quote_ttl, snapshot.as_of + binding.max_rate_age)`;
- **recomputes** `rate_applied_num/den`, `fee_amount`, `net_source_amount`, `destination_amount`
  and the residual from the referenced `fx_rate_snapshots` row and the currently active
  `conversion_pair_policies` version, using the DS-1 arithmetic of item 6 implemented in SQL
  `NUMERIC`, and **refuses any mismatch**;
- checks the snapshot's ingest seal (S-B2).

Updates are limited to the `status` transitions (`quoted→executed|expired|rejected`). Quote ids
come from `gen_random_uuid()`. Reconciliation R1 recomputes from the snapshot and policy rows,
**never from the stored rate**.

#### S-B2 (security blocker): ingest seal

`fx_rate_snapshots` rows (and `sb_market_results`, Part C) carry a Go-computed ingest seal following
the ADR 0111 T10 pattern. The seal is checked by the quote guard (and by `SettleFromResult`). This
is recorded as an **ADR 0110 T5/T10-class residual**: platform-service identities are only a GUC on
`igaming_runtime`, and arbitrary SQL with stolen credentials is inside the ADR 0110 threat model. It
is **LAUNCH-BLOCKING for any non-MOCK rate source or feed.**

### B.3 Data model

```
fx_rate_source_bindings (platform; dual-control writes like asset registry 0044/0047)
  id, source_kind ('mock' only CHECK until an ADR adds a real one), base_asset, quote_asset, priority,
  max_rate_age, max_deviation_bp, history_lookback, issues_provider_reference BOOL, active (platform kill switch), version

fx_rate_snapshots (platform market data; append-only; readable by tenant sessions; NULL-ARM fence, writer fx_rate_ingest)
  id, binding_id, base_asset, quote_asset, rate NUMERIC CHECK (rate > 0) (unconstrained scale, exact),
  rate_precision SMALLINT, as_of CHECK (as_of <= fetched_at), fetched_at, provider_reference NULL,
  plausibility_baseline NUMERIC NULL, source_kind, ingest_seal BYTEA NOT NULL; UNIQUE (binding_id, as_of)

conversion_pair_policies (tenant RLS, versioned; narrow-only under platform bindings, ADR 0037 §A.5)
  id, tenant_id, source_asset, destination_asset, version, effective_from, enabled,
  min_source_amount, max_source_amount NUMERIC(38,0) NOT NULL, fee_bp INT CHECK (0..10000), fee_min,
  spread_bp INT CHECK (0..10000), quote_ttl CHECK (quote_ttl <= interval '15 seconds'),
  max_outstanding_quotes INT NOT NULL, kill_switch BOOL, created_by, approved_by (four-eyes on fee/spread)

conversion_quotes (tenant RLS + player_self_scope SELECT; S-B1 guard)
  id DEFAULT gen_random_uuid(), tenant_id, player_account_id, source_wallet_id, destination_wallet_id NULL,
  source_asset, destination_asset, source_amount, fee_amount, net_source_amount CHECK (> 0), destination_amount,
  snapshot_id, direction ('direct','inverse'), spread_bp, rate_applied_num, rate_applied_den NUMERIC(38,0),
  policy_id, policy_version, rounding_rule_id, residual_num, residual_den NUMERIC,
  status ('quoted','executed','expired','rejected'), expires_at, created_at, terminal_at
  CHECK (source_asset <> destination_asset); CHECK (source_amount = fee_amount + net_source_amount)

conversion_operations (ADR 0021 shape + ADR 0037 B.5 additions; append-only)
  id, tenant_id, player_account_id, source/destination wallet ids and asset codes, source_amount,
  destination_amount, rate_applied_num/den, direction, spread_bp, rate_source (binding id), rate_timestamp,
  fee_amount, fee_asset_code, provider_reference, rounding_rule_id, residual_num/den,
  quote_id UNIQUE, snapshot_id, idempotency_key, ledger_transaction_id UNIQUE, created_at
  UNIQUE (tenant_id, player_account_id, idempotency_key)
  guard: both wallets' player_account_id = row's; assets match; every value equals the quote's
```

**One ledger-finance migration plus Go slice (B0) for type `conversion`:**
- (a) Redefine `ledger_transactions_transaction_type_check` from 20 to 21 values (migration 0091
  pattern) and add `ledger.TxConversion`.
- (b) `ledger_transactions_check1`: `conversion` requires `reason_code IS NULL` and
  `reverses_transaction_id IS NULL`.
- (c) A commit-time shape trigger in the WD043 style: exactly the B.4 entries, plus exactly one
  `conversion_operations` row with the matching `ledger_transaction_id`.
- (d) Add `fx_conversion_clearing` and `conversion_fee_revenue` to the `account_type` CHECK, with
  `CHECK (wallet_id IS NULL)` for both (the converse of 0132 `player_types_have_wallet`). A trigger
  lets only `conversion` touch them. `conversion_fee_revenue` is house-level and credit-normal,
  never `house_gaming` (OQ-B3).
- (e) ADR 0116's `aml_monitorable_tx_type` includes `conversion` literally. The 0116 DB feed is
  authoritative; the outbox event is supplementary.
- (f) The `internal/risk/cumulative.go` specs classify `conversion` explicitly, as excluded from
  stake and exposure.
- (g) Reconciliation type sets gain `conversion`, plus the `fx_conversion` stream.
- (h) `internal/idempotency/identifiers.go` reserves the `conversion:` prefix.
- (i) The 0118/0129 status-gate `WHEN` lists include `conversion` as a DB backstop. Projection
  rebuild is account-generic, so it needs no change.

### B.4 Posting shape (one transaction, ledger idempotency key `conversion:<quote_id>`)

| Asset | Entries |
|---|---|
| Source A | Dr `player_cash`(A) `source_amount`; Cr `fx_conversion_clearing`(A) `net_source_amount`; Cr `conversion_fee_revenue`(A) `fee_amount` (omitted when 0) |
| Destination B | Dr `fx_conversion_clearing`(B) `destination_amount`; Cr `player_cash`(B) `destination_amount` |

Spread P&L and the residual land in the clearing position. They are never posted as a fractional
leg. Back Office labels the position "unrealised FX + spread".

### B.5 Execute transaction (ADR 0082 R8 order)

One `WithTenant` player transaction:
1. Status-gate advisories, tenant first, then brand.
2. Risk and RG at L0.
3. L1: `conversion_quotes` `FOR UPDATE` (a new L1 table, placed after `payment_attempts`).
4. Expiry check with `clock_timestamp()` **after** the L1 lock. The quote must be
   `status = 'quoted'`, belong to the session player, have an active policy, and have an
   `active` platform binding. If it has expired, set it to `expired` and **commit** with result
   `QUOTE_EXPIRED`.
5. **Deviation guard (OQ-B4).** If the newest snapshot for the pair has moved more than
   `max_deviation_bp` either way from the quote's snapshot, refuse with `RATE_MOVED`. Execute
   **never re-prices**.
6. Re-check `CheckEligibility`, the B.2 item 9 wallet restrictions, and player account status.
   Then create the destination wallet and the ledger accounts.
7. L3: `ledger.LockProjectionsForPosting`, then check `player_cash(A) ≥ source_amount`.
8. L4: `ledger.Post`.
9. INSERT `conversion_operations`, mark the quote `executed`, call `audit.Record`, then
   `outbox.Emit(conversion.executed)` at L5.

**Idempotency.** A replay with the same `Idempotency-Key` and the same quote returns the original.
The same key with a **different `quote_id`** returns `IDEMPOTENCY_PAYLOAD_MISMATCH`. The same key
from a different player, or for a quote the player does not own, returns **409**, never another
player's record. The ledger key `conversion:<quote_id>` is the backstop.

### B.6 API and screens

- **Player API** at `/v1/wallet/conversions`:
  - `POST /quotes {source_wallet_id, destination_asset, source_amount}`. Tenant and player come
    from the session. It is **rate-limited per player** and capped by
    `max_outstanding_quotes`.
  - `POST / {quote_id}` with an `Idempotency-Key` header.
  - `GET /` and `GET /{id}`. Another player's quote or operation returns **404**.
  - Closed rejection codes: `RATE_UNAVAILABLE`, `RATE_STALE`, `RATE_IMPLAUSIBLE`, `RATE_MOVED`,
    `PAIR_DISABLED`, `ASSET_INELIGIBLE`, `WALLET_RESTRICTED`, `QUOTE_EXPIRED`,
    `INSUFFICIENT_FUNDS`, `AMOUNT_OUT_OF_RANGE`, `LIMIT_EXCEEDED`,
    `IDEMPOTENCY_PAYLOAD_MISMATCH`.
- **B2C:** a conversion dialog showing a countdown on the quote, and the conversion history.
- **Back Office (tenant):**
  - a list of conversions, and a detail view showing the quote, snapshot, policy version, residual
    and a link to the ledger transaction;
  - the pair policy editor (four-eyes) and the per-pair kill switch;
  - the FX reconciliation view;
  - the clearing position, labelled "unrealised FX + spread".
- **Platform console:**
  - rate source bindings (dual control; `active` is the platform kill switch);
  - a snapshot viewer;
  - a plausibility-rejection log.

### B.7 Reconciliation (stream `fx_conversion`, hourly)

These checks replace the earlier R3:
- **R1.** Recompute rate, fee, destination amount and residual from the **snapshot and policy
  rows**, never from the stored rate. They must equal the stored values.
- **R2.** The ledger entries must match the B.4 shape exactly.
- **R3a.** Per operation, the ledger clearing amounts must equal B.4 recomputed from the persisted
  exact rate.
- **R3b.** For each (tenant, asset), the `fx_conversion_clearing` balance must equal
  `Σ net_source credited − Σ destination debited` over `conversion_operations`.
- **R4.** There must be no orphan conversion transaction or orphan operation.
- **R5.** Projection drift is covered by the existing checks.

Any non-zero result is a P1. The open FX position is **reported, never asserted to be zero**. A GGR
test asserts that conversion fee and spread are excluded from GGR.

### B.8 Invariants

- **FX-1:** cross-asset value moves only as a final `conversion` transaction with exactly one
  operation row.
- **FX-2:** each quote executes at most once.
- **FX-3:** both wallets belong to the same player and hold different assets.
- **FX-4:** no float and no scaled NUMERIC rate is ever a posting input. The rate is an exact
  `num/den`, rounded once.
- **FX-5:** every quote is DB-recomputed at insert (S-B1), and every conversion is reproducible from
  the snapshot and policy (R1).
- **FX-6:** a stale, absent, implausible, moved, unsealed or unconfigured rate means refusal.
- **FX-7:** `MockRateSource` is refused in production.
- **FX-8:** a restricted wallet can never be converted out of.

### B.9 Failure modes and tests (ledger-finance's 12-category rule)

Tests:
- **Normal path.** Conversions at exponent pairs 2→2, 2→8, 8→6, and exponent 0. Inverse direction.
  A spread at 22 or more decimals stays exact.
- **Rounding.** An exact half rounds away from zero. Residuals are signed and stored exactly. The
  unit-scaling regression from LF-B-ii is covered.
- **Forgery (S-B1).** A quote INSERT with a tampered rate, fee, amount, `expires_at`, status or
  `created_at` is refused. An unsealed snapshot is refused.
- **Duplicates and concurrency.** Same-key replay returns the original. Same key with another quote
  returns `IDEMPOTENCY_PAYLOAD_MISMATCH`. Another player's key or quote returns 409 or 404.
  Concurrent executes of one quote produce one posting. Concurrent conversions never overdraw.
- **Refusals.** Every B.2 item 9 and 10 condition, `RATE_MOVED`, a platform binding with
  `active = false`, the outstanding-quote cap, and an exponent-18 asset.
- **Finality.** A `conversion` with `reverses_transaction_id` set is refused by the DB.
- **Partial failure.** Any partial failure rolls back the whole transaction.
- **Reconciliation.** Fixtures for R1, R3a and R3b.
- **Lock order.** A lock-order harness covers B.5.
- **Feeds.** The AML feed (0116) includes `conversion`.

### B.10 Slices (owners)

- **B0** (ledger-finance): migration (a)-(i) and the decision record. **Must land first.**
- **B1** (backend): `internal/fx`, the MOCK source, a sealed ingest worker, bindings, and the ADR
  0037 B.6/B.7 checks.
- **B2** (ledger-finance): quote with the S-B1 guard, execute, posting, finality, idempotency, and
  the L1 amendment.
- **B3** (backend + frontend): API and screens.
- **B4** (ledger-finance + backend): reconciliation R1-R5.
- **Exponent-18 assets stay DISABLED until LF-16b.**

**Reviews:**
- ledger-finance (owner): done with conditions, applied here.
- security: done with conditions, applied here.
- At implementation: qa and code-reviewer.

**Open item for compliance:** OQ-B5 (§E).

## Part C: Sportsbook feed abstraction (provider-neutral, MOCK feed)

### C.1 Verified context

- **Provider interface.** `sportsbook.Provider` has only `Catalogue(ctx)`
  (`internal/sportsbook/types.go:370-372`).
- **Catalogue data.** The `sb_*` tables are platform-wide. They hold current odds only, with no
  version history.
- **Bet placement.** Placement checks odds by exact equality (`orchestrator.go:232`). It reads
  event, market and selection status without locking those rows (`orchestrator.go:216-230`).
- **Settlement.** Settlement (`settlement.go:327`) is driven only by the
  `simulate-settlement-event` route (`internal/httpserver/sportsbook_settlement_handlers.go:20-27`).
- **Prior design.** doc 09 §4.1 and §1.4 are made concrete here, not redesigned.

### C.2 Decision

1. **New `FeedProvider` interface** in `internal/sportsbook/datafeed`, alongside the unchanged
   `Provider.Catalogue`. No adapter call is made while a transaction is held (ADR 0095).
   ```go
   type FeedProvider interface {
       Capabilities(ctx context.Context) (FeedCapabilities, error) // push|poll|both, kinds, versioning mode, heartbeat cadence
       Poll(ctx context.Context, cursor FeedCursor, limit int) (FeedBatch, error)
       Health(ctx context.Context) (FeedHealth, error)
   }
   type FeedMessage struct {
       FeedID, MessageID string // dedupe key
       Kind       FeedKind      // fixture | event_state | market_state | price | result | heartbeat
       EntityRef  ExternalRef   // mapped via doc 09 §2.5
       Version    uint64        // per-entity monotonic per Capabilities.VersioningMode
       ProducedAt time.Time
       Body       FeedBody      // canonical only
   }
   ```
   - An adapter must declare `entity_sequence` or `timestamp_sequence`; an adapter that supports
     neither is not admissible.
   - The design assumes nothing about how a real provider behaves.
   - Cursors and feed fields are never used as URLs.
   - **Polling endpoints** come only from compiled adapter config, or from dual-control platform
     config with an HTTPS host allowlist. Private address ranges are denied, cross-host redirects
     are refused, and timeouts and response-size caps apply.
2. **Push admission (security condition).** Push uses a new platform-scope ADR 0097 admission domain
   `sportsbook_feed`:
   - its own signing prefix and headers;
   - a platform credential held in secretstore, not per-tenant `providercred`;
   - **mTLS or a source allowlist ahead of the pre-auth buckets**, with a dedicated pre-auth bucket
     for the feed;
   - rate buckets keyed by `feed_id`;
   - a required `ReplayTimestampWindow`;
   - then `(feed_id, message_id)` dedupe and version monotonicity.
3. **Ingestion.** `ingest.Apply` processes one message per transaction under
   `sportsbook_feed_ingest`. It:
   - deduplicates the message;
   - maps references; unmapped references are recorded, never created;
   - applies a message only if `Version > feed_version`, and records it as `stale_dropped`
     otherwise;
   - appends history and seals result rows (S-B2);
   - emits the platform events (L5).
4. **Result validation and the mandatory hold (OQ-C2, security + ledger-finance).**
   - A result must name **exactly the market's own selections**. Otherwise it is
     `rejected_invalid` and alerted.
   - The result is **held** in `pending_confirmation` when any of the following is true:
     - the total cross-tenant payout on the market, or any single payout, exceeds the platform
       threshold, or exceeds the per-tenant, per-asset minor-unit threshold;
     - a correction raises the payout;
     - the result arrives before the event's `start_time`;
     - the market was not suspended or closed under C0 `FOR UPDATE` before the result was applied.
   - A held result posts nothing: the stake stays in `player_locked_cash`.
   - Confirmation is an **ADR 0110 actor-proof-bound four-eyes** operation.
   - If a tenant has no configured threshold, **feed-driven settlement is disabled for that
     tenant** (fail closed).
   - MOCK may set the threshold to 0, which turns hold-all off.
5. **Settlement fan-out (OQ-C3, ledger-finance idempotency).**
   - `sb_settlement_dispatch` is an ordered consumer, partitioned by `market_id`.
   - It takes the tenant set from the **`tenants` table, never from feed data**. The set includes
     non-active tenants that have open bets. Their refusals are recorded, not dropped.
   - It records the state of each `(result_id, bet_id)` item in `sb_settlement_dispatch_items`.
   - For each bet, under the bet's L1 lock, it re-reads the market's **latest** `result_version`
     and compares it with the bet's last applied `result_id`:
     - unsettled → settle at generation 1;
     - a different outcome → rollback(g) and settle(g+1) **in one transaction**, where g comes from
       the stored max + 1, never from the event;
     - the same outcome under a newer version → a non-financial **adoption row**;
     - a lower version → no-op;
     - void result → void. Void is **terminal** (ADR 0088), so a correction after a void raises an
       alert for manual handling.
   - Payout always comes from the bet's frozen odds.
   - A refused bet (for example, inactive tenant) marks its item and **never dead-letters the
     market partition**.
   - A delivery reaches `done` only after every tenant pass succeeds.
   - Settlement checks the result seal. `driver='feed'` and `actor_staff_account_id` is NULL.
6. **State messages** apply suspend, resume, close, cancel and void. Staff trading operations
   (suspend, void event, manual result) are an audited second source and are **ADR 0110
   actor-proof operations**. Manual results require four-eyes.
7. **Bet acceptance.** These checks are added before RG, and existing check order is unchanged:
   - (a) `ExpectedOddsVersion` must equal `odds_version`.
   - (b) Price-change policy is `exact` by default. `accept_better` is allowed **only with the C0
     rule**: the frozen odds, potential return, L0.6 exposure and risk all use the **live** price
     read under C0, which is recorded as `accepted_odds_version`.
   - (c) A stale feed heartbeat or stale live price means refuse. Unconfigured staleness also means
     refuse.
   - (d) A pre-match market past `start_time` is refused.
   - (e) Event, market and selection rows are read `FOR SHARE` as lock class **C0**. This is an
     **explicit ADR 0082 R8 exception**: C0 is taken before all L0-L4 locks, and no settlement,
     staff or watchdog path locks C0 after any L0-L4 lock. **Required check:** confirm with
     security which grants `FOR SHARE` on `sb_*` needs (it requires UPDATE privilege).
   - New rejection: `RejectionFeedStale`.
8. **Watchdog.**
   - Heartbeat loss suspends the feed's live markets. Each suspension is audited and **raises an
     alert**.
   - Markets resume only on an **authenticated fresh state message** or a staff action.
   - The watchdog has its own allowlisted identity with a single-call-site test.
9. **Cashout** is a hook only and is NOT IMPLEMENTED. A reserved `CashoutPricer` needs its own ADR.
10. **Simulate route.** It stays unchanged, and is registered only when `Environment != production`
    AND the test-support flag is set (`Load()` refuses that combination in production). Any
    production settlement row with `driver='simulation'` is a **P1 reconciliation mismatch**.

### C.3 Data model (platform-wide unless stated)

```
sb_feed_sources: feed_id PK, kind ('mock' only CHECK until an ADR), status, versioning_mode, endpoint_config_ref,
                 last_heartbeat_at, last_message_at, poll_cursor, heartbeat_max, suspend_after, live_price_max_age
sb_feed_messages: (feed_id, message_id) PK, kind, entity_type, entity_id NULL, version, payload_sha256, produced_at,
                  received_at, outcome ('applied','stale_dropped','duplicate','unmapped','rejected_invalid','held')
sb_events / sb_markets / sb_selections: + feed_id NULL, feed_version, state_updated_at; selections + odds_version, price_updated_at
sb_price_history (append-only): selection_id, odds_version, num, den, source, feed_message ref, effective_at; UNIQUE (selection_id, odds_version)
sb_state_changes (append-only): entity, from/to status, source ('feed','staff','watchdog'), reason_code, actor_staff_id, actor_proof_id, at
sb_market_results (append-only, sealed): id, market_id, result_version, source, selection_outcomes, is_void,
                  status ('applied','pending_confirmation','confirmed','rejected'), ingest_seal, confirmation actor proofs;
                  UNIQUE (market_id, result_version)
sb_settlement_dispatch_items (tenant RLS): result_id, bet_id, state ('done','refused','adopted','noop','alerted'), code; PK (result_id, bet_id)
sb_settlement_payout_thresholds (tenant RLS, per asset, minor units) + platform threshold row
sportsbook_bets (tenant): + accepted_odds_version
sportsbook_bet_settlements (tenant): + result_id NULL, + driver ('simulation','feed','staff') NOT NULL DEFAULT 'simulation'
sportsbook_price_change_policies (tenant RLS, versioned): ('exact','accept_better')
```

Dead-heat and fractional outcomes are NOT IMPLEMENTED. A message carrying one is rejected and
raises an alert.

### C.4 Invariants

- **SBF-1:** an entity's state only moves forward in `feed_version`.
- **SBF-2:** each message is applied at most once.
- **SBF-3:** a feed message never posts money. Only `settleBet`, `voidBet` and `rollbackBet` post,
  and only under L1.
- **SBF-4:** payout always comes from the bet's frozen odds.
- **SBF-5:** a correction is a rollback plus a resettle in one transaction. Generation g is derived
  from the database. A void is terminal.
- **SBF-6:** a stale or unconfigured feed means no new bets.
- **SBF-7:** odds history is append-only.
- **SBF-8:** the tenant scope comes from the `tenants` table and the bet row, never from the feed.
- **SBF-9:** a held, unsealed, or selection-mismatched result posts nothing.
- **SBF-10:** a refused bet never blocks its market partition.

### C.5 Reconciliation

New checks:
- `sb_unsettled_after_result`
- `sb_outcome_vs_result`
- `sb_settlement_stale_result`
- `sb_simulation_driver_in_production`, a P1

The MOCK-statement check is kept. The Back Office read route belongs to gap-audit item 9.

### C.6 Failure modes and tests

Tests, by area:
- **Ingestion:**
  - dedupe and out-of-order versions;
  - unmapped references;
  - malformed messages;
  - push and poll produce the same result;
  - an SSRF set: private range, redirect, oversize response, URL taken from a cursor.
- **Admission:**
  - a bad signature, a replayed timestamp, or a source not on the allowlist is refused before any
    work happens;
  - a flood of feed traffic cannot exhaust tenant buckets.
- **Watchdog:**
  - heartbeat loss suspends markets and raises an alert;
  - an unauthenticated message cannot resume a market.
- **Locking:**
  - the C0 race;
  - a deadlock harness including C0 and the R8-exception static check;
  - `accept_better` freezes the live price for exposure and risk.
- **Results:**
  - each hold trigger fires individually;
  - confirmation without four-eyes is refused;
  - a missing threshold disables feed settlement;
  - a result whose selection set does not match is refused;
  - an unsealed result is refused.
- **Fan-out:**
  - two tenants, plus an inactive tenant with open bets (refusal recorded, partition not blocked);
  - a crash mid-fan-out redelivers without double-posting;
  - a lower version is a no-op;
  - a same-outcome correction writes an adoption row;
  - a correction after a void raises an alert.
- **Simulation:** a production `simulation` row is a P1.

### C.7 Slices (owners)

- **C1** (sportsbook + architect): the interfaces, a deterministic MOCK feed, ingest with seals,
  NULL-arm fences, and the allowlisted identities. No settlement in this slice.
- **C2** (sportsbook, with ledger-finance review): placement rules, the C0 amendment as an R8
  exception, a check of the grants C0 needs, and the C0 rule for `accept_better`.
- **C3** (ledger-finance + sportsbook; needs A1): results, hold and confirmation, the dispatch
  consumer and its items, `SettleFromResult`, and C.5.
- **C4** (sportsbook + frontend): the push-admission domain, watchdog alerts, Back Office trading
  operations as actor-proof operations, and the catalogue fetch timeout.
- **C5** (separate ADR): cashout, multiples, and a real adapter. The seal residual is
  launch-blocking here.

**Reviews:** security and ledger-finance reviewed with conditions, and those conditions are applied
above. qa and code-reviewer review at implementation.

## D. Sequencing, ADR 0082 amendments, residuals

**Sequencing:**
- Track A: A1 → A2 → A3.
- Track C: C1 (in parallel with A1) → C2 → C3 (needs A1) → C4.
- Track B: B0 **first** → B1 → B2 (needs A1) → B3/B4.

Only B2 and C3 post money, and both are owned by ledger-finance.

**ADR 0082 amendments:**
- **L5**, for `Emit`, which runs after the partition's serialising lock.
- **C0**, for catalogue rows `FOR SHARE`/`FOR UPDATE`. This is an explicit R8 exception: C0 comes
  first, and no path takes C0 after any L0-L4 lock.
- **`conversion_quotes`** added to L1, after `payment_attempts`. B.5 order: status-gate advisories
  → L0 → L1 → create → L3 → L4 → L5.

The orchestrator owns the amendment sections.

**Residual (ADR 0110 T5/T10 class):** a stolen-credential SQL path can bypass platform-service GUC
identities. The ingest seals (S-B2) mitigate this. The residual is **launch-blocking for any
non-MOCK rate source or feed**. Live rates and real feeds remain PROVIDER DEPENDENT.

## E. Owner questions: orchestrator defaults (reversible by the named owner)

| # | Owner | Default in force |
|---|---|---|
| OQ-A1 | human/compliance | 90-day retention for delivered events (DB-held, 30-day floor), aligned with HQ-E1-4, which remains open |
| OQ-A2 | security | Yes. Four-eyes is required to skip or disable `financial`/`compliance` subscriptions. All operator actions are actor-proof-bound |
| OQ-B1 | ledger-finance | Clearing per (tenant, asset), as ADR 0021. A future treasury sweep gets its own transaction type, never `manual_adjustment` |
| OQ-B2 | ledger-finance | DS-1 half-up with ties away from zero, applied once. The operator bears the residual via clearing. The signed residual is stored exactly |
| OQ-B3 | ledger-finance | A new house-level, credit-normal `conversion_fee_revenue` account, never `house_gaming` |
| OQ-B4 | human/commercial | Ledger-finance position: TTL ≤ 15 s, `max_source_amount` mandatory (NULL = refused), execute refuses with `RATE_MOVED` beyond `max_deviation_bp` and never re-prices. Fee and spread levels are commercial; default is 0 with the pair disabled |
| OQ-B5 | compliance | Compliance decides. The minimum is inclusion in the AML feed (0116 item e). Any withdrawal or KYC value threshold must aggregate across assets per person, which needs a separate reporting-rate decision |
| OQ-C1 | product/human | `exact` by default. `accept_better` only with the C0 live-price rule |
| OQ-C2 | security/ledger-finance | Hold is MANDATORY. Above a per-tenant, per-asset threshold (or the platform triggers in C.2.4) a result goes to `pending_confirmation`, which posts nothing, and is confirmed by four-eyes with actor proof. An unconfigured threshold disables feed settlement. MOCK may use 0 = off |
| OQ-C3 | architect | Enumerate tenants from `tenants`, including non-active tenants with open bets. Refusals are recorded. A delivery is `done` only after every tenant pass. Revisit above 50 tenants |

## F. Review disposition (2026-10-12)

| ID | Reviewer | Finding | Disposition |
|---|---|---|---|
| S-B1 | security (blocker) | Quote forgery mints money | Applied: B.2 S-B1 recompute guard; R1 from snapshot and policy |
| S-B2 | security (blocker) | Platform identity is only a GUC | Applied: ingest seals on snapshots and results. Residual is launch-blocking for non-MOCK (§D) |
| S-B3 | security (blocker) | Operator and subscription actions are unbound | Applied: A.5 actor-proof operations, four-eyes for financial/compliance, relay prohibitions (OB-10) |
| S-C1 | security | Relay contradicts the 0114 pattern | Applied: A.2.3 claim-only relay, tenant UPDATE policy, NULL-tenant path, NULL-ARM-WRITE-1 |
| S-C2 | security | Events trusted as facts; free-text payloads; dedupe | Applied: A.2.5, A.4 hash compare, OB-8/OB-9 |
| S-C3 | security | Retention executable by runtime | Applied: A.5 owner-owned function, DB window with a floor |
| S-C4 | security | FX authorisation gaps | Applied: B.5 idempotency scope and 409, platform kill switch, account status, 404, rate limit and quote cap |
| S-C5 | security | Feed push admission | Applied: C.2.2 |
| S-C6 | security | Hold, selection set, watchdog DoS | Applied: C.2.4, C.2.8 |
| S-C7 | security | Tenant enumeration source | Applied: C.2.5 |
| S-L1..L4 | security (low) | Simulate route, SSRF, identity allowlists, actor-proof trading | Applied: C.2.10, C.2.1, A.3/C.2.8, C.2.6 |
| LF-B-i | ledger-finance (blocker) | Scaled NUMERIC rate is inexact | Applied: B.2 item 5 num/den with an exact guard |
| LF-B-ii | ledger-finance (blocker) | Unit error in `RoundToMinorUnits` input | Applied: B.2 item 6 |
| LF-C1 | ledger-finance | B.5 lock order and clock | Applied: B.5, §D |
| LF-C2 | ledger-finance | Type `conversion` change list | Applied: B.3 (a)-(i) |
| LF-C3 | ledger-finance | R3 unsound | Applied: B.7 R3a/R3b, position reported not asserted |
| LF-C4 | ledger-finance | Wallet eligibility | Applied: B.2 item 9 |
| LF-C5 | ledger-finance | Finality and corrections | Applied: B.2 item 3, B.3(b) |
| LF-C6 | ledger-finance | Idempotency payload mismatch | Applied: B.5 |
| LF-C7 | ledger-finance | Outbox partition and OB-7 wording | Applied: A.2.6, A.6 |
| LF-C8 | ledger-finance | Fan-out idempotency and generations | Applied: C.2.5 |
| LF-C9 | ledger-finance | C0 as an R8 exception; `accept_better` pricing; grants | Applied: C.2.7(e)/(b); grant check is a C2 task with security |
| LF-L1..L3 | ledger-finance (low) | GGR exclusion, no reporting reuse, BO label, exponent-18 disabled | Applied: B.2 items 7 and 11, B.4, B.7, B.10 |
