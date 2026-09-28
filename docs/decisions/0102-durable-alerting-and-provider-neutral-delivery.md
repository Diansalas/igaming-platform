# ADR 0102 — Durable Alerting and Provider-Neutral Delivery (ALERT-DELIVERY-1)

- **Status:** PROPOSED, 2026-09-28. Drafted by `architect` for PRH-2 W0 (workstream W0-X). Nothing
  in this ADR is implemented. The implementation is PRH-2 workstreams **I-core** (W1) and
  **I-wire** (W4).
- **Decision type:** cross-domain architecture: the new `internal/alerting` package, migration
  **0110**, and the `payments`, `reconciliation`, `httpserver` and `cmd/platform-api` call sites.
- **Owner:** `architect` (design); `devops` and backend (implementation).
  **Reviewers:** `security` (every section, hard gate), `ledger-finance` (§7, the in-tx rule),
  `code-reviewer`, `qa` (§11). `product-owner-proxy` has already noted that alerting is proportionate
  (plan §10, PO F4–F8).
- **Registry:** ALERT-DELIVERY-1 (this ADR). PAY-P1-MULTISUCCESS-ALERT-1 closes with it,
  **except** that it stays launch-blocking until HD-PRH2-4-OPS configures real recipients.
- **Binding inputs:**
  - `docs/plans/prh2-hardening-round/plan.md` §4 row 0110, §5-I, §5.0 T-1/T-2, §11 (HD-PRH2-4);
  - ADR 0098 §5 (HD-PRH2-4);
  - `reviews/security.md` S-7 items 1–4 and the Addendum §1, conditions (a)–(e);
  - `reviews/ledger-finance.md` LF-7;
  - `reviews/qa.md` F2/F3 and the W1/W4 checklists;
  - `reviews/code-reviewer-verification.md` I4/I6.
- **Related:** ADR 0013 (audit dual-scope RLS), ADR 0081 §3.2 (closed platform-service
  vocabulary), migrations 0084 (`app.platform_service_id`), 0105 (validated session resolver) and
  0106 (mixed-GUC exclusion), ADR 0095 §28.4/§28.8/§28.9, ADR 0104 (the `subject_tenant_id`
  pattern).
- **Labels:** every item below is a design decision (engineering, reversible) unless it is marked
  **HUMAN DECISION** (none is new here) or **SECURITY RULING REQUESTED**. No recipient, person,
  address, phone number or on-call rota is named or seeded anywhere (HD-PRH2-4).

---

## 1. Context (verified at `cabca27`)

- Every alert today is a log line only; there is no alert table. The sites were verified by reading
  the code, and the full list is in §8.
- The live multiple-success P1 is `auditMultipleSuccessForIntent`
  (`internal/payments/orchestrator.go:989-1024`, Error log at `:1018`). It runs inside the T10/T13d
  evidence transaction, which must commit with its receipt (LF95-C3, ADR 0095 §28.4).
- The backstop line `payments_deposit_intent_index_backstop_fired` (`:1021`) is in the same function.
- **Reconciliation `MISMATCH FOUND` lines (`reconciliation/scheduler.go:333-341`, `:420`, `:487`,
  `:562`, `:719`) are emitted after the per-tenant run transaction has committed.** The windows are
  disjoint `[now-interval, now]` (`RunSchedulerLoop`, `:600`):
  - ledger-vs-projection drift (`reconciliation.go:108`) is a **full-state** recompute, so the next
    run re-detects it;
  - the sportsbook, casino-consistency, casino-statement and payment-statement streams are
    **period-bound**. A run that is lost or rolled back for a window is not re-covered by the next
    window.
- The kill-switch engage alert (`payments_kill_switch_handlers.go:360`, called at `:653`) is logged
  after the engage transaction commits.
- The handler `*_integrity_alert_*` lines fire on the **error path**, after the callback
  transaction has rolled back. Several of them log the raw `err` value.
- **Correction to the plan inventory (for the orchestrator).** Three sites sit on routes that are
  registered only behind simulation flags, not just `payment_deposit_simulation_handlers.go:240`:
  - `casino_play_handlers.go:242-288`, behind `CasinoPlaySimulationEnabled` (`casino_routes.go:39-43`);
  - `sportsbook_settlement_handlers.go:185`, behind `SportsbookSettlementSimulationEnabled`
    (`sportsbook_routes.go:68-69`);
  - `payment_deposit_simulation_handlers.go:240` itself.

  Plan §5-I's rule for `:240` (test-support route → simulation Kind, never paged) therefore applies
  to all three. §8 applies it.
- The platform-service GUC exists: `db.WithPlatformService` (`internal/db/platform_service.go:65`)
  sets `app.platform_service_id` from a closed, compiled-in allowlist, and migration 0084's policies
  check it. Adding a member requires an ADR plus a migration (ADR 0081 §3.2). This ADR adds one
  (§6).
- `internal/observability` exposes an OTel meter (`metrics.go:17`).
- `providerref.Fingerprint` (`providerref.go:94`) yields a 12-hex-character SHA-256 prefix.

## 2. Decision summary

1. **Durability.** Alerts are durable rows. Delivery is asynchronous, through a provider-neutral
   `Sink`. No tenant-scoped or financial transaction ever does outbound I/O for an alert.
2. **Scope.** Every alert is either **tenant-owned** (`tenant_id = X`) or **platform-owned**
   (`tenant_id IS NULL`).
   - A platform-owned alert about a tenant carries `subject_tenant_id = X`, and X can read it
     read-only. This is the ADR 0104 pattern.
   - Every integrity P1 is platform-owned. A tenant cannot acknowledge, resolve or suppress it (S-7.2).
3. **Dedup.** UNIQUE `(tenant_id, dedup_key)` NULLS NOT DISTINCT, over non-resolved alerts. The
   `dedup_key` is a generated column that always embeds the Kind and the subject tenant, so it
   cannot collide across tenants (S-7.1).
4. **Payloads.** Each Kind has an attribute allowlist, enforced in Go and in the DB. Provider refs
   appear only as fingerprints. There is no PII, no secret or token material and no raw error text
   (S-7.3).
5. **Routing (HD-PRH2-4).** A versioned `alert_routes` table maps (severity, scope, escalation step)
   to a channel kind plus an **opaque recipient reference**.
   - **The migration seeds no routes.**
   - With no route, an alert is `unrouted`: visible, counted, and itself raising a platform
     warning.
6. **Delivery.** Delivery and escalation state are append-only rows in `alert_deliveries`. Retry
   uses bounded backoff on an injectable clock (T-1). The dispatcher runs under
   `WithPlatformService("alert_dispatcher")`, and has read on alerts and insert on deliveries only
   (S-7.4). §6.3 records the one narrow exception, flagged for a security ruling.
7. **The in-transaction rule** (LF-7 plus the security addendum (a)–(e)):
   - a savepoint;
   - a narrow allowlist-based swallow, where 25P02, serialization and deadlock errors propagate;
   - a mandatory post-commit detached re-raise;
   - the metric `alert_raise_failures_total{kind}`;
   - a per-Kind backstop table (§7.5).

   **No Kind in the PRH-2 inventory uses abort-on-failure** (§7.6).

## 3. Model

### 3.1 Kinds, severity, scope

- **Kinds.** `Kind` is a closed vocabulary. It is defined in Go (`alerting.Kinds`) and mirrored in
  the immutable reference table `alert_kinds`, which only a migration writes. Each Kind fixes:

  | Field | Meaning |
  |---|---|
  | `severity` | One of `p1`, `p2`, `p3` |
  | `scope` | `platform` or `tenant` |
  | `simulation` | Whether the Kind is simulation-only |
  | `allowed_keys` | The attribute allowlist |
  | `in_tx_raisable_by_tenant` | Whether a tenant session may raise it (§5) |
  | `backstop` | Documentation only; see §7.5 |

- **Severity** is fixed per Kind and is never chosen at the call site.
  - `p1` means integrity or money-correctness, and follows CLAUDE.md ("any non-zero drift is a P1").
  - `p2` means an operational safety event, such as an engaged kill switch or an alerting meta-warning.
  - `p3` is informational. Every simulation Kind is `p3`.
- **Simulation Kinds** are prefixed `simulation.` and are never delivered.
  - A CHECK requires `simulation ⇒ severity = 'p3'`.
  - A trigger on `alert_deliveries` refuses any delivery row other than `suppressed_simulation` for
    a simulation alert (defence in depth). The dispatcher never selects them either.
- **Adding a Kind** requires a migration row plus a Go entry plus a review. It is never a runtime
  string.

### 3.2 Tables (migration 0110)

**`alert_kinds`** is reference data.
- Columns: `kind` PK; `severity`; `scope`; `simulation`; `allowed_keys TEXT[]`;
  `in_tx_raisable_by_tenant BOOL`.
- It is seeded with the §8 Kinds. Seeding vocabulary is not seeding recipients.
- UPDATE, DELETE and TRUNCATE are denied by trigger.
- It is SELECT-able by every session. It holds no tenant data, so it needs no RLS family. It is
  still `ENABLE`/`FORCE` with a single `FOR SELECT USING (true)` policy, so that no write policy
  exists.

**`alerts`**: one row per open deduplicated condition.

| Column | Notes |
|---|---|
| `id` | UUID PK |
| `tenant_id` | UUID NULL, FK `tenants`. NULL means platform-owned |
| `subject_tenant_id` | UUID NULL, FK `tenants`. `CHECK (subject_tenant_id IS NULL OR tenant_id IS NULL)` |
| `kind` | TEXT NOT NULL, FK `alert_kinds` |
| `severity` | TEXT NOT NULL; the trigger forces it from `alert_kinds` |
| `simulation` | BOOL NOT NULL; forced from `alert_kinds` |
| `discriminator` | TEXT NOT NULL, `^[A-Za-z0-9:_.-]{1,160}$`. Built from entity ids only (e.g. `intent:<uuid>`, `run:<uuid>`, `switch:<uuid>`), never a raw provider reference |
| `dedup_key` | TEXT `GENERATED ALWAYS AS (kind \|\| '\|' \|\| COALESCE(subject_tenant_id::text,'-') \|\| '\|' \|\| discriminator) STORED` |
| `attributes` | JSONB NOT NULL. A flat object of scalar values, ≤ 2 KiB, keys ⊆ `alert_kinds.allowed_keys` (trigger) |
| `state` | `open` / `acked` / `resolved` |
| `acked_by`, `acked_at`, `resolved_by`, `resolved_at`, `resolve_reason_code` | Forced by trigger from the session actor |
| `first_seen_at` | |
| `created_at` | |

- **Dedup index:** `UNIQUE NULLS NOT DISTINCT (tenant_id, dedup_key) WHERE state <> 'resolved'`.
  Raising again after a resolve creates a **new** alert row, so there is no "reopen" UPDATE path.
- **Scope-consistency trigger:** a Kind whose scope is `platform` requires `tenant_id IS NULL`; a
  Kind whose scope is `tenant` requires `tenant_id IS NOT NULL`.

**`alert_occurrences`**: append-only, one row per `Raise`.
- Columns: `id`; `alert_id` FK; `tenant_id` and `subject_tenant_id`, copied by trigger from the
  alert; `raised_at`; `raised_by_scope` (`tenant` | `platform_admin` | `platform_service` | `system`).
- The occurrence count is `count(*)`. `last_seen_at` is `max(raised_at)`. **There are no counter
  UPDATEs**, which is what lets a tenant session raise a platform-owned alert without any UPDATE
  power (§5).

**`alert_routes`** holds the routing configuration: versioned, effective-dated and append-only with
supersession.

| Column | Notes |
|---|---|
| `id` | |
| `scope` | `platform` \| `tenant` |
| `tenant_id` | NULL, and always NULL in PRH-2 (see §4.3) |
| `severity` | |
| `escalation_step` | SMALLINT ≥ 0 |
| `channel_kind` | `CHECK IN ('log','mock')` in PRH-2 (§6.2) |
| `recipient_ref` | See below |
| `escalate_after` | INTERVAL NULL |
| `effective_from`, `superseded_at`, `superseded_by` | Supersession |
| `created_by` | Forced from the validated platform-admin GUC |
| `created_at` | |

- **`recipient_ref`** is an opaque reference key, not an address:
  `CHECK (recipient_ref ~ '^[a-z0-9][a-z0-9_.:-]{0,127}$')`. It refuses `@`, `+`, digits-only phone
  shapes and whitespace, so an email or phone number cannot be stored here even by mistake.
  Resolving a reference to a real contact is a `RecipientResolver` concern (§6.2), and the real
  recipients are HD-PRH2-4-OPS.
- **No seed rows.** A migration test asserts that `alert_routes` has zero rows after `up`.

**`alert_deliveries`**: append-only. It holds delivery and escalation state.

| Column | Notes |
|---|---|
| `id` | |
| `alert_id` | FK |
| `tenant_id`, `subject_tenant_id` | Copied by trigger |
| `escalation_step` | |
| `attempt_no` | |
| `event` | `claimed`, `sent`, `failed`, `unrouted`, `dead`, `suppressed_simulation` |
| `route_id` | NULL |
| `channel_kind` | NULL |
| `last_error_class` | An enum, `CHECK IN ('timeout','unavailable','rejected','misconfigured','unknown')`, NULL unless `event = 'failed'`. **Never raw error text** (S-7.3) |
| `next_attempt_at` | NULL |
| `next_escalation_at` | NULL |
| `recorded_at` | |

- `UNIQUE (alert_id, escalation_step, attempt_no, event)` makes multi-instance claims safe:
  inserting the `claimed` row is the claim.
- The **current delivery state** of an alert is its latest `alert_deliveries` row. It is a
  projection, not an updated column.

All five tables: `ENABLE` + `FORCE ROW LEVEL SECURITY`. There are UPDATE/DELETE/TRUNCATE deny
triggers on `alert_kinds`, `alert_occurrences`, `alert_routes` (except the one-way supersession
columns, under a whole-row-equality guard) and `alert_deliveries`. `alerts` allows UPDATE only
through the state-guard trigger (§4.2).

## 4. Scope, ownership and acknowledgement

### 4.1 RLS families (migration 0110; the families are named)

Every family predicate uses the `NULLIF(current_setting('<guc>', true), '')` form. Every
non-platform family also requires `app.platform_admin_principal_id` **and**
`app.platform_service_id` unset, and every non-player family requires `app.player_account_id`
unset. This is the 0106 mixed-GUC exclusion.

| Family (policy name) | Table(s) | Predicate | Grants |
|---|---|---|---|
| `alerts_tenant_owned` | `alerts`, `alert_occurrences`, `alert_deliveries` (SELECT only) | `tenant_id = app.tenant_id` | Tenant sessions: SELECT, INSERT, and UPDATE through the state guard (tenant-owned only) |
| `alerts_subject_tenant_read` | `alerts`, `alert_occurrences` | `tenant_id IS NULL AND subject_tenant_id = app.tenant_id` | **FOR SELECT only.** This is the G1/ADR 0104 pattern. Tenant sessions do **not** see `alert_deliveries` for platform-owned alerts, because platform routing references are not tenant data |
| `alerts_subject_tenant_raise` | `alerts`, `alert_occurrences` | `tenant_id IS NULL AND subject_tenant_id = app.tenant_id` | **FOR INSERT only**, WITH CHECK. The trigger additionally requires `alert_kinds.in_tx_raisable_by_tenant` and forces `state='open'` and NULL ack/resolve fields |
| `alerts_platform_admin` | all five | Validated `app.platform_admin_principal_id` (a `staff_users` row with `tenant_id IS NULL`; the 0105 resolver pattern), with tenant, player and service unset | SELECT all. INSERT platform-owned alerts and occurrences. UPDATE the state of platform-owned alerts through the guard. INSERT and supersede `alert_routes` |
| `alerts_platform_service_dispatcher` | all five | `app.platform_service_id = 'alert_dispatcher'`, with tenant, player and platform-admin unset | **SELECT** on `alert_kinds`, `alerts`, `alert_occurrences`, `alert_routes`; **INSERT** on `alert_deliveries`. **No UPDATE or DELETE anywhere** (S-7.4). The one exception is §6.3 |

- **ON CONFLICT and RLS.** `Raise` uses `INSERT … ON CONFLICT DO NOTHING`, which needs no UPDATE
  policy. Because `dedup_key` embeds the subject tenant, a tenant session can conflict only with a
  row that its own `alerts_subject_tenant_read` or `alerts_tenant_owned` family already exposes. So
  the S-7.1 concern (a collision with an invisible row, leaking the key's existence or erroring in
  a financial transaction) cannot arise. This is TI-tested (§11).

### 4.2 Acknowledgement and resolution (S-7.2)

- **Platform-owned alerts.** Only `alerts_platform_admin` may ack or resolve them.
  - A tenant session has no UPDATE policy on them. An attempted UPDATE affects zero rows, and the
    handler reports that as 404/403.
  - **A tenant acknowledgement therefore can never suppress the platform's view or delivery of an
    integrity P1.**
- **Tenant-owned alerts.** Tenant staff may ack or resolve them through `WithPrincipalScope`, and
  the platform may also view them. A tenant ack of a tenant-owned alert has no effect on any
  platform-owned alert.
- **The state guard** (a BEFORE UPDATE trigger):
  - identity, payload and dedup columns are immutable;
  - the allowed transitions are `open→acked`, `open→resolved` and `acked→resolved`, and nothing
    else;
  - the actor is forced from the session: the validated platform principal, or the 0105-validated
    tenant principal;
  - `resolve_reason_code` is required on resolve.
- **Every ack and resolve is an audited staff action.** An ack or resolve of a platform-owned alert
  carrying `subject_tenant_id` is audited with `audit_log.subject_tenant_id` = that tenant
  (ADR 0104), so the subject tenant sees who handled its P1.
- **Escalation stops at ack.** The dispatcher does not escalate an `acked` alert. Resolving stops
  all delivery.

### 4.3 Deliberately not built in PRH-2

- Tenant-authored routes (`alert_routes.scope='tenant'` rows) are not built, because no
  tenant-owned Kind exists in the §8 inventory. The column and CHECK exist so that adding them needs
  no schema change. A follow-up is registered in §12.
- There is no route write API beyond platform-admin; see §12 for whether it needs four-eyes.

## 5. Raise API (I-core)

```go
package alerting

type Alert struct {
    Kind            Kind
    OwnerTenantID   uuid.UUID            // uuid.Nil ⇒ platform-owned
    SubjectTenantID uuid.UUID            // required for platform-owned tenant-subject Kinds
    Discriminator   string               // entity ids only
    Attributes      map[string]AttrValue // validated against the Kind allowlist before any SQL
}

func Raise(ctx context.Context, tx pgx.Tx, a Alert) error        // strict; any error returned
func RaiseGuarded(ctx context.Context, tx pgx.Tx, a Alert) error // §7 savepoint rule
func WithDeferred(ctx context.Context) (context.Context, *Deferred)
func (d *Deferred) Flush(ctx context.Context, r TxRunner)        // post-commit detached re-raise
func RaiseDetached(ctx context.Context, r TxRunner, a Alert) error
```

- **Session scope.** `Raise` inserts into whichever family the caller's transaction already has.
  - A tenant transaction may raise tenant-owned Kinds, and platform-owned Kinds whose subject is
    its own tenant.
  - A platform-admin transaction may raise platform-owned Kinds.
  - `Raise` never opens or changes a GUC.
- **Mechanics.** It performs `INSERT … ON CONFLICT DO NOTHING RETURNING id`. If that returns no row,
  it SELECTs the existing non-resolved row by `(tenant_id, dedup_key)`, then INSERTs the
  occurrence. If a concurrent resolve races the SELECT, it retries at most twice.
- **Detached raises.** `RaiseDetached` opens a fresh transaction under `WithTenant(subject or
  owner)`, or `WithPlatformService("alert_dispatcher")` for a platform-owned Kind with no subject
  tenant.
  - It uses a detached, bounded context: the `deniedAuditCtx` pattern at
    `payments_kill_switch_handlers.go:334`, so a client disconnect never skips it.
  - It makes at most 3 attempts, with backoff on the injected `Clock`.
- **Attribute validation in Go** happens before any SQL:
  - an unknown key, a non-scalar value or a value over its bound is refused;
  - a provider-reference attribute must be of type `ProviderRefFingerprint`, which can only be
    constructed by `providerref.Fingerprint`;
  - there is no `error` attribute type at all.

  A Go-side validation failure is a programming error: it is logged at Error, counted, and caught
  by the per-Kind constructor unit tests (§11).

## 6. Delivery (I-core function; wired in I-wire)

### 6.1 Dispatcher

- **Identity.** A new closed-allowlist member, `db.ServiceAlertDispatcher = "alert_dispatcher"`,
  recorded here as the ADR-level decision ADR 0081 §3.2 requires. Migration 0110 widens only the
  alert-table policies (§4.1) to accept it.
- **Loop.**
  - It reads due work in a short read-only `WithPlatformService` transaction and commits.
  - It calls `Sink.Deliver` **with no transaction open**. A txscope guard refuses otherwise, as the
    E3/ADR 0095 adapters do.
  - It then records the outcome in a fresh short transaction, by INSERT only.
- **"Due" work** is any open, non-simulation alert whose latest delivery row is one of:
  - absent;
  - `failed` with `next_attempt_at ≤ now`;
  - `unrouted` (re-evaluated every pass, so an alert becomes deliverable as soon as a route is
    configured);
  - `sent` with `next_escalation_at ≤ now` and the alert not acked.

  **`now` comes from the injected `Clock`**, which is passed as a query parameter and never taken
  from the DB's `now()` (T-1). Tests set it explicitly; there is no `time.Sleep`.
- **Retry.** Bounded exponential backoff, with the parameters in config (technical defaults,
  reversible). After `max_attempts` for a step, it inserts `dead`, emits an Error meta-log and
  increments `alert_dead_total{channel_kind}`. It raises the meta Kind `alerting.delivery_dead`
  (§6.3).
- **Escalation.** When a route for step `n` has `escalate_after`, the `sent` row carries
  `next_escalation_at`. If the alert is still `open` at that time, step `n+1`'s route applies.
  With no route for `n+1`, the step is recorded as `unrouted`.
- **Unrouted (HD-PRH2-4).** With no effective route for (severity, scope, step), the dispatcher:
  1. inserts one `unrouted` row per (alert, step), made idempotent by the UNIQUE constraint;
  2. increments `alert_unrouted_total{severity}`;
  3. raises the platform warning `alerting.unrouted`.

  `alerting.unrouted` is platform-owned, p2, has discriminator `severity:<p1|p2|p3>`, and gets one
  occurrence per newly unrouted alert. It is deliberately not one alert per unrouted alert, to
  avoid a storm.

  The original alert stays `open`, visible in the platform alert list, and counted. This is the
  state until HD-PRH2-4-OPS configures routes.
- **Multi-instance.** Inserting the `claimed` row is the claim; the loser of the UNIQUE race skips.
  A crash between `claimed` and the outcome row causes one re-send after lease expiry. Sinks
  receive the idempotency key `<alert_id>:<step>:<attempt_no>`. Delivery is therefore
  at-least-once, and this is stated rather than hidden.
- **Wiring.** I-core ships the dispatcher as an unwired function. I-wire adds the one `main.go` line
  after H merges (plan §3 Rule 5).

### 6.2 Channel interface

```go
type Sink interface {
    ChannelKind() ChannelKind // "log" | "mock" in PRH-2
    Deliver(ctx context.Context, d Delivery) (Outcome, ErrorClass)
}
type Delivery struct {
    IdempotencyKey  string
    AlertID         uuid.UUID
    Kind            Kind
    Severity        Severity
    Scope           Scope
    SubjectTenantID uuid.UUID
    Attributes      map[string]AttrValue // already allowlisted
    RecipientRef    string               // opaque; the sink resolves it via RecipientResolver
}
type RecipientResolver interface { Resolve(ctx context.Context, ref string) (Recipient, error) }
```

The implementations are:

| Sink | Label | Behaviour |
|---|---|---|
| `LogSink` | **IMPLEMENTED on merge** | Structured, allowlisted Error line |
| `MockSink` | **MOCK** | Records deliveries in memory. It can be told to fail with a given `ErrorClass` or time out, for the retry and dead tests |

- A real channel (email, SMS, webhook, paging vendor) is **PROVIDER DEPENDENT** and out of scope
  (plan §8). It is added later by a migration widening the `channel_kind` CHECK, an ADR amendment,
  and a real `RecipientResolver` once HD-PRH2-4-OPS supplies contacts.
- No real `RecipientResolver` exists in PRH-2. The mock resolver returns a synthetic recipient and
  refuses anything that is not a `mock:`-prefixed reference.

### 6.3 SECURITY RULING REQUESTED: the dispatcher's meta-alert insert

- **The conflict.** HD-PRH2-4 requires an unrouted alert to raise a platform warning, and the
  dispatcher is what discovers that an alert is unrouted. S-7.4 limits the dispatcher to "read on
  alerts, insert on deliveries only".
- **Proposed default.** The `alerts_platform_service_dispatcher` family also gets **FOR INSERT** on
  `alerts` and `alert_occurrences`, with a WITH CHECK restricted to:
  - `tenant_id IS NULL AND subject_tenant_id IS NULL`;
  - `kind IN ('alerting.unrouted','alerting.delivery_dead')`.

  It still has no UPDATE and no DELETE.
- **Alternative.** No DB alert. The warning is only the metric, the Error log, and a derived "N
  unrouted" figure on the platform alert list. This keeps S-7.4 literal, but the warning is then
  not a durable alert.
- **Security to choose before I-core merges.**

## 7. The in-transaction rule (LF-7 plus security addendum §1 (a)–(e))

### 7.1 Where `RaiseGuarded` is used

`RaiseGuarded` is used in every business transaction that raises a Kind, including:
- the financial evidence and posting transactions (T10, T13d, any posting);
- the reconciliation run transactions;
- the kill-switch engage transaction.

The business outcome is never made to depend on the alert row. For a financial transaction, the
dispute or receipt record is itself the fail-closed outcome (security addendum §1).

### 7.2 Mechanics (conditions (a) and (c))

1. `SAVEPOINT` (pgx nested `tx.Begin`). If creating the savepoint fails, **propagate**: the outer
   transaction is already unusable.
2. Run `Raise` inside the savepoint.
3. If `Raise` fails, `ROLLBACK TO SAVEPOINT`. **If the rollback-to-savepoint fails, propagate.**
4. **Narrow swallow (a).** The error is swallowed **only** if it is a `*pgconn.PgError` whose
   SQLSTATE is in this allowlist:
   - class `22` (data exception);
   - class `23` (integrity constraint violation);
   - `42501` (insufficient privilege, including an RLS WITH CHECK failure);
   - `P0001` (a trigger RAISE).

   **Everything else propagates**, including:
   - `25P02` (already-aborted outer transaction);
   - `40001` (serialization);
   - `40P01` (deadlock);
   - `57014` (cancel);
   - classes `08` and `53`;
   - context cancellation;
   - any non-PG error.

   Propagating is safe: evidence application and reconciliation are idempotent and retried
   (provider redelivery, sweeper, the next scheduler tick).
5. **On a swallow (c):**
   - log at Error `alert_raise_failed` with `kind` and `sqlstate_class` only;
   - increment `alert_raise_failures_total{kind,phase="in_tx"}`. The labels are bounded, with **no
     tenant label**; `phase` ∈ {`in_tx`, `detached`};
   - register the alert on the context's `*Deferred`.

   Neither the log nor the metric is a precondition for the money path: a meter or logger failure
   is ignored (the J pattern).

### 7.3 Post-commit detached retry (condition (b))

- **Collector.** Every call site's caller establishes `ctx, deferred := alerting.WithDeferred(ctx)`
  before opening the business transaction.
- **After commit.** Only after `WithTenant`/`WithPlatformAdmin`/… returns nil (the transaction has
  committed), the caller runs `deferred.Flush(detachedCtx, pool)`, which calls `RaiseDetached` for
  each swallowed alert.
  - If the business transaction rolled back, `Flush` is not called for **event** alerts: a
    rolled-back event raises no alert.
- **The detached retry is MANDATORY for every Kind in PRH-2** (see §7.5 for why this is stricter
  than condition (b) requires).
  - A detached failure is logged at Error and counted with `phase="detached"`.
  - It is never retried beyond its bounded attempts.
- **A missing collector is a programming error.** A `RaiseGuarded` swallow with no `*Deferred` on
  the context logs `alert_detached_unscheduled` at Error and counts it. An AST/unit test asserts
  that every I-wire call site establishes a collector.

### 7.4 Failure-path P1s (LF-7(2), ADR 0095 §28.8)

- **These use `RaiseDetached` directly.** A failure-path P1 is one whose business transaction
  **rolls back** because the rollback is the outcome: every handler `*_integrity_alert_*` site in
  §8, which runs after `WithTenant` has returned the error.
- The detached raise runs in a fresh `WithTenant(t.ID)` transaction, in the same place as the
  existing separately committed rejection/denial records (e.g. `recordCasinoCallbackRejection`).
- "A rolled-back event raises no alert" applies to **event** alerts only.

### 7.5 Per-Kind reconciliation backstops (condition (d))

"Backstop" means a standing reconciliation check that **re-surfaces the condition by itself** if
the P1 row is lost. A durable business record that someone can query is listed, but it does not
count as a backstop.

| Kind (§8) | Standing reconciliation backstop | Detached retry |
|---|---|---|
| `payment.multiple_success_for_intent` | **Conditional:** `pay_captured_unposted` and `pay_duplicate` (`reconciliation/payment_statement.go:138,148`), only for a provider/period with a wired payment-statement source. Today every source is MOCK. | **Mandatory** (the backstop is not universally present) |
| `payment.deposit_intent_index_backstop_fired` | Same conditional backstop. The disputed attempt row (terminal reason `multiple_success_for_intent`) is durable in the same transaction. | **Mandatory** |
| `reconciliation.ledger_projection_drift` | **Yes:** the drift sweep itself. It is a full-state recompute, so the next tick re-detects any drift that persists. | **Mandatory** in PRH-2 (uniformity; see below) |
| `reconciliation.sportsbook_settlement_mismatch`, `.casino_consistency_mismatch`, `.casino_statement_mismatch`, `.payment_statement_mismatch` | **None.** The windows are disjoint and period-bound, so the next run does not re-cover the window. The `reconciliation_mismatches` rows are durable in the same transaction, but no check re-raises them. | **Mandatory** |
| `payment.kill_switch_engaged` | None. The switch row and the audit row are durable in the same transaction. | **Mandatory** |
| Handler integrity Kinds (`casino.callback_integrity.*`, `payment.webhook_integrity.*`) | Partial: `casino_consistency` `cas_*` kinds and `pay_*` statement kinds, where a source exists. Rejection and denial records exist for some (`recordCasinoCallbackRejection`; the `deposit_handlers.go:465` denial audit). | Detached is the **primary** path (§7.4), with bounded retries |
| `simulation.*` | N/A (never delivered) | Detached, best-effort |
| `alerting.unrouted`, `alerting.delivery_dead` | Re-derived every dispatcher pass | Re-raised by the next pass |

**Why every Kind is mandatory.** Condition (b) makes the detached retry mandatory only for Kinds
without a backstop. PRH-2 makes it mandatory for every Kind, because:
- the only true backstop (drift) exists for one Kind;
- the payment backstop depends on a real statement source that does not exist yet.

One uniform rule is also simpler to test and to mutate. This is stricter than the addendum, never
looser.

### 7.6 Abort-on-failure for non-financial integrity alerts: decided NO for every PRH-2 Kind

The security addendum permits abort-on-failure for non-financial integrity alerts. This ADR does
not use it for any current Kind:
- **Reconciliation runs.** Aborting would roll back the run and its `reconciliation_mismatches`
  rows. For the four period-bound streams, that permanently loses the durable evidence for the
  window (§1), which is strictly worse than committing the evidence with a deferred alert. For
  drift, it would lose the run record.
- **The kill-switch engage.** An alerting failure must never prevent the safety brake from
  engaging.
- **Handler integrity sites.** Their business transaction has already rolled back (§7.4), so there
  is nothing to abort.

A future Kind may use abort-on-failure only if this ADR is amended with a reason, and only for a
business action that is (i) retried by its caller and (ii) not a safety control. **Security to
confirm this ruling at ADR review.** It reverses security's original S-7.5 recommendation for the
non-financial class, for the reasons above.

## 8. I-wire site list (verified at `cabca27`)

**Order of wiring:** plan §5-I. **Logs are retained:** the existing log line stays, and the Raise
is additional.

| # | Site | Kind (severity, scope) | Raise mode | Attribute allowlist |
|---|---|---|---|---|
| 1 | `payments/orchestrator.go:989-1024` (`auditMultipleSuccessForIntent`, log at `:1018`), post-E2 | `payment.multiple_success_for_intent` (p1, platform, subject = attempt tenant) | `RaiseGuarded` in T10/T13d | `deposit_intent_id`, `attempt_id`, `evidence_kind`, `provider_id`. **No amount, asset or reference in the alert**; those stay in the audit row, per security F-L2 |
| 2 | `:1021` (`…_index_backstop_fired`) | `payment.deposit_intent_index_backstop_fired` (p1, platform) | `RaiseGuarded`, same tx | `deposit_intent_id`, `attempt_id` |
| 3 | `reconciliation/scheduler.go:333-341` (drift) | `reconciliation.ledger_projection_drift` (p1, platform) | `RaiseGuarded` **moved inside** the per-tenant run tx (`:285`); the post-commit log line stays | `run_id`, `mismatch_count`, `stream` |
| 4 | `scheduler.go:420` | `reconciliation.sportsbook_settlement_mismatch` (p1) | Same, inside that stream's run tx | `run_id`, `mismatch_count`, `stream`, `statement_source` |
| 5 | `scheduler.go:487` | `reconciliation.casino_consistency_mismatch` (p1) | Same | `run_id`, `mismatch_count`, `stream` |
| 6 | `scheduler.go:562` | `reconciliation.casino_statement_mismatch` (p1) | Same | `run_id`, `mismatch_count`, `stream`, `statement_source` |
| 7 | `scheduler.go:719` | `reconciliation.payment_statement_mismatch` (p1) | Same, inside the match tx (`WithTenantSnapshot`) | `run_id`, `mismatch_count`, `statement_source`, `provider_id`, `import_id` |
| 8 | `payments_kill_switch_handlers.go:360` (called at `:653`) | `payment.kill_switch_engaged` (**p2**, platform, subject = `c.target`, route-validated) | `RaiseGuarded` **moved inside** the engage tx (`:596`), which is platform-admin or tenant-principal scope; the log stays | `kill_switch_id`, `provider_scope`, `operation_scope`, `reason_code`, `changed_by_scope`, `is_platform_takeover`. The actor id is in the audit row, not the alert |
| 9 | `casino_handlers.go:495,510,522,536,556` | `casino.callback_integrity.{bet_not_found, provider_round_ownership_conflict, payload_mismatch, original_tombstoned, win_origin}` (p1, platform) | `RaiseDetached` (§7.4) | `provider_id`, `request_id`. **Never `err`** |
| 10 | `deposit_handlers.go:451,465,518` | `payment.webhook_integrity.{payload_mismatch, deposit_already_reversed, reversal_link}` (p1, platform) | `RaiseDetached` | `provider_id`, `request_id` |
| 11 | `payment_deposit_simulation_handlers.go:240` | `simulation.payment.payload_mismatch` (p3, simulation) | `RaiseDetached`; **never delivered** | `request_id` |
| 12 | `casino_play_handlers.go:242,252,261,276,288` (**simulation-gated**, §1) | `simulation.casino_play.<reason>` (p3, simulation) | `RaiseDetached`; never delivered | `action`, `request_id` |
| 13 | `sportsbook_settlement_handlers.go:185` (**simulation-gated**, §1) | `simulation.sportsbook_settlement.<reason>` (p3, simulation) | `RaiseDetached`; never delivered | `bet_id`, `reason`, `event_type`, `generation`, `bet_status`, `request_id`. The ADR 0088 §4.4 set, minus the staff actor id, which stays in the log and audit |

- **Removed site.** `orchestrator.go:1509` dies with E2 and is not wired.
- **E2 precondition.** Row 1 is wired only after E2 merges (plan §2).

## 9. Migration 0110 (I-core): content

1. **`alert_kinds`**: table, seed rows for §8's Kinds plus `alerting.unrouted` and
   `alerting.delivery_dead`, the immutability triggers, `FORCE RLS`, and a SELECT-only policy.
2. **`alerts`**: the §3.2 columns, the CHECKs, the generated `dedup_key`, the partial UNIQUE
   NULLS NOT DISTINCT index, the scope-consistency and forced-severity triggers, the state-guard
   trigger, the attribute trigger (flat scalar object, ≤ 2 KiB, keys ⊆ `allowed_keys`), and the
   indexes `(tenant_id, state, created_at)` and `(subject_tenant_id, state, created_at) WHERE
   subject_tenant_id IS NOT NULL`.
3. **`alert_occurrences`**, **`alert_routes`**, **`alert_deliveries`**: per §3.2, with the
   append-only triggers (UPDATE/DELETE per row, TRUNCATE per statement, the 0014/0016 pair), the
   `last_error_class` enum CHECK, the `recipient_ref` CHECK, and the simulation-delivery refusal
   trigger.
4. **`ENABLE` + `FORCE ROW LEVEL SECURITY`** on all five, and the §4.1 families by name:
   `alerts_tenant_owned`, `alerts_subject_tenant_read`, `alerts_subject_tenant_raise`,
   `alerts_platform_admin` and `alerts_platform_service_dispatcher`. The last includes the §6.3
   INSERT, if security accepts it.
5. **Grants:** append-only least-privilege lines in `deploy/init-app-role.sql` (plan §3 Rule 4). No
   role, password or attribute change.
6. **No `alert_routes` rows.**
7. **Down:** `RAISE EXCEPTION` if any row exists in `alerts`, `alert_occurrences`,
   `alert_deliveries` or `alert_routes`. Otherwise drop in reverse order.

## 10. Invariants (for `qa` and `code-reviewer`)

| ID | Invariant |
|---|---|
| AL-1 | An alert row, occurrence or delivery is visible only to its owner tenant, its subject tenant (read-only, and not deliveries), the validated platform admin, and the dispatcher service. RLS enforces this, not Go filters. |
| AL-2 | No tenant session can change the state of a platform-owned alert. |
| AL-3 | `dedup_key` always embeds the Kind and the subject tenant. The UNIQUE constraint is keyed on `tenant_id`. There is no cross-tenant conflict. |
| AL-4 | Attributes ⊆ the Kind allowlist. Provider refs appear only as 12-hex fingerprints. There is no PII, secret or token material, and no raw error text. `last_error_class` is an enum. |
| AL-5 | No transaction is open during `Sink.Deliver`. |
| AL-6 | In a business transaction, `Raise` runs under a savepoint. Only allowlisted SQLSTATEs are swallowed; 25P02, 40001 and 40P01 always propagate. |
| AL-7 | Every swallowed in-transaction raise gets a post-commit detached raise. A rolled-back business transaction never flushes its event alerts. |
| AL-8 | No migration seeds a route or recipient. An unrouted alert stays open, counted and warned. |
| AL-9 | Simulation Kinds are never delivered. |
| AL-10 | The dispatcher cannot UPDATE or DELETE any alert table. Its INSERT is limited to deliveries, plus the §6.3 meta-Kinds if accepted. |

## 11. Tests and mutants (T-1 injectable clock; T-2 no wall-clock assertion; T-3 local runs never labelled CI)

**I-core:**
- **IDM/CON:** N concurrent raisers of the same Kind and discriminator → 1 alert row and N
  occurrences. Raising after a resolve → a new alert row.
- **TI:**
  - tenant A's raise never conflicts with or reveals B's (same Kind and discriminator, different
    subject);
  - A cannot read B's subject alerts;
  - A cannot see deliveries of platform-owned alerts;
  - A cannot raise with subject = B (42501).
- **RLS/AZ:**
  - a tenant UPDATE of a platform-owned alert affects 0 rows;
  - a tenant ack of a tenant-owned alert leaves platform-owned alerts untouched;
  - a mixed-GUC session (tenant + platform admin, or tenant + service) sees nothing and writes
    nothing;
  - the dispatcher's UPDATE is refused, and so is its INSERT of a non-meta Kind;
  - a player session sees nothing.
- **Payload:**
  - an unknown attribute key is refused in Go and by the DB trigger;
  - a raw provider reference cannot be constructed as `ProviderRefFingerprint` (compile-time/unit);
  - an oversized payload is refused;
  - `last_error_class` outside the enum is refused.
- **Routing and delivery** (clock-driven):
  - no route → an `unrouted` row, `alerting.unrouted` raised, and the metric incremented;
  - a route added later → delivered on the next pass;
  - `MockSink` failing → retries at clock-set times → `dead` + `alerting.delivery_dead`;
  - not acked by `next_escalation_at` → step 1;
  - acked → no escalation;
  - a simulation alert → no delivery; a direct INSERT of a non-suppressed delivery row is refused;
  - two dispatchers → one `claimed` per (alert, step, attempt);
  - a txscope test proves no transaction is open during `Deliver`.
- **MIG:**
  - zero `alert_routes` rows after `up`;
  - `recipient_ref` refuses `ops@example.invalid` and `+15550100`;
  - `down` refuses while rows exist.
- **The in-tx rule** (addendum (e) and LF I):
  - an injected `Raise` failure (a P0001 via a test trigger) inside T10/T13d → the dispute and
    receipt commit, the **uniform 200** is returned, `alert_raise_failures_total{kind,phase="in_tx"}`
    increments, and **the post-commit detached `Raise` persists the alert**;
  - an already-aborted outer transaction (25P02) → **propagates**, not masked; a 40001 or 40P01
    injected inside the savepoint → propagates;
  - a failure-path P1 whose business transaction rolls back → the detached alert persists;
  - a rolled-back event → no alert.

**I-wire:**
- Every §8 site raises its Kind with exactly its allowlisted attributes. For the reconciliation
  sites, this is asserted from within the run transaction: a run row and an alert row, or neither
  plus a detached one.
- The multiple-success alert is durable in the same transaction as the refusal record (QA W4).
- Rows 11–13 are never delivered.
- The existing log lines still fire (**R**).

**Mutants (must be killed):**

| Mutant | Must be killed by |
|---|---|
| `RaiseGuarded` without a savepoint | the injected-failure test fails with 25P02 on the next statement |
| Swallowing every error, including 25P02, 40001 and 40P01 | the propagation tests |
| Dropping the post-commit `Flush` | the detached-persistence test |
| Removing `subject_tenant_id` from `dedup_key` | the TI dedup test |
| `alerts_subject_tenant_read` without the player/platform exclusions | the mixed-GUC and player tests |
| Granting the dispatcher UPDATE | the RLS test |
| Deleting the simulation delivery guard | the simulation test |
| Deleting the attribute trigger | the DB payload test |
| Seeding one route | the MIG test |

## 12. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| Global `UNIQUE (dedup_key)` | The S-7.1 cross-tenant collision and existence leak. |
| Counter columns on `alerts`, updated by `ON CONFLICT DO UPDATE` | A tenant session would need UPDATE on platform-owned rows, which is the S-7.2 suppression risk. An append-only occurrences table avoids it. |
| Delivery and escalation state as UPDATEd columns on `alerts` | This contradicts S-7.4 (the dispatcher is insert-only). An append-only `alert_deliveries` projection is used instead. |
| `SECURITY DEFINER` raise function | The app role owns the tables and FORCE RLS applies to the owner (ADR 0013), so definer rights grant nothing without a role or attribute change, which is forbidden. |
| Dispatcher under `WithoutTenant` | The S-7.4 requirement; `WithoutTenant` is the generic platform read scope (ADR 0081 §3.2). |
| Abort-on-failure for reconciliation and kill-switch Kinds | §7.6. |
| Swallow by denylist (propagate only 25P02, 40001, 40P01) | Allowlisting the swallowable classes is narrower (condition (a)). |
| Seeding placeholder routes or recipients | HD-PRH2-4 forbids fictional recipients. |
| Implementing a real email or paging channel now | Out of scope (plan §8); PROVIDER DEPENDENT. |

## 13. Consequences

- P1s become durable and visible to the platform, and to the subject tenant read-only. Nobody is
  paged until HD-PRH2-4-OPS configures routes **and** a real channel exists. PAY-P1-MULTISUCCESS-ALERT-1
  stays launch-blocking.
- The payments, reconciliation and kill-switch call sites gain a context collector plus a
  post-commit flush. The business outcome of every transaction is unchanged.
- There is one new platform-service identity and five new tables under FORCE RLS.

## 14. Open items

1. **SECURITY RULING REQUESTED:** §6.3 (the dispatcher's meta-alert INSERT, or the metric-only
   alternative).
2. **Security to confirm §7.6** (no abort-on-failure for any PRH-2 Kind).
3. **Orchestrator: plan inventory correction.** Rows 12 and 13 are simulation-gated routes; §1
   applies §5-I's `:240` rule to them.
4. **Inventory candidates not wired.** Both are registered for the orchestrator, not silently
   added:
   - the `reconciliation sweep: tenant run failed` Error lines (`scheduler.go:314,407,473,547`);
   - the payment-statement failure line `tenant run failed (P1)` (`scheduler.go:647`), which the
     code already labels a P1.

   A future `reconciliation.run_failed` Kind is recommended.
5. **Tenant-owned Kinds and tenant-authored routes:** none exist in PRH-2 (§4.3). Register a
   follow-up if the partner console needs them.
6. **Route write API:** platform-admin-only and audited. Whether a route change needs four-eyes is
   for security to decide. The default is single platform admin plus audit, because routes move no
   money.
7. **Real channels and resolvers:** PROVIDER DEPENDENT; HD-PRH2-4-OPS.
8. **Retry and backoff parameters and `max_attempts`** are technical defaults in config, to be
   reviewed by devops.

**Handover/DoD:**
- **Artefacts:** this ADR ACCEPTED after security review, then:
  - I-core merged with migration 0110, `internal/alerting` and the §11 I-core tests;
  - I-wire merged with the §8 rows, the dispatcher line in `main.go` and the §11 I-wire tests;
  - `docs/runbooks/observability-and-alerting.md` updated, with the routing matrix as an explicit
    placeholder pending HD-PRH2-4-OPS;
  - HANDOVER mock-vs-real row: `LogSink` IMPLEMENTED; `MockSink` MOCK; real channels PROVIDER
    DEPENDENT.
- **Registry (orchestrator):** ALERT-DELIVERY-1 → IMPLEMENTED (MOCK/log channels).
  PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking on HD-PRH2-4-OPS.
