# ADR 0102 — Durable Alerting and Provider-Neutral Delivery (ALERT-DELIVERY-1)

> **Status 2026-09-28: ACCEPTED** (revision 2). Ledger-finance and security accepted it: see `docs/plans/prh2-hardening-round/reviews/` (`adr-0102-0104-security-confirmation.md`, and for 0102 also the confirmation section of `adr-0102-ledger-finance.md`); product-owner-proxy accepted it in `adr-0102-0104-product-owner-proxy.md`. Implementation is NOT IMPLEMENTED until the PRH-2 workstream merges. The earlier PROPOSED status below is historical.

- **Status:** PROPOSED, **revision 2**, 2026-09-28. Drafted by `architect` for PRH-2 W0 (W0-X).
  Nothing here is implemented. The implementation is PRH-2 **I-core** (W1) and **I-wire** (W4).
- **Revisions:**

  | Rev | Base | Change |
  |---|---|---|
  | 1 | `f06818b` | Initial draft |
  | 2 | `6864efa` | Applies all conditions from the reviews listed below, plus the orchestrator's binding resolutions and security's `raise_failed` ruling (`adr-0099-0101-security.md` Part 1). Mapped in §15 |

  Reviews applied in revision 2 (all under `docs/plans/prh2-hardening-round/reviews/`):
  - `adr-0102-ledger-finance.md`: ACCEPT WITH CONDITIONS, F1–F15 and 12 required tests;
  - `adr-0102-0104-security.md`: ACCEPT WITH CONDITIONS, SR-1..8, Q1–Q3, C-102-1..9, and the
    orchestrator dispositions;
  - `adr-0102-0104-product-owner-proxy.md`: ACCEPT;
  - `adr-0099-0101-security.md` Part 1: `alerting.raise_failed` ACCEPTED, with conditions.

- **Decision type:** cross-domain architecture: new `internal/alerting`, migration **0110**, and call
  sites in `payments`, `reconciliation`, `httpserver` and `cmd/platform-api`.
- **Owner:** `architect` (design); `devops` and backend (implementation).
  **Reviewers:** `security` (hard gate), `ledger-finance` (§7), `code-reviewer`, `qa` (§11).
- **Registry:** ALERT-DELIVERY-1. PAY-P1-MULTISUCCESS-ALERT-1 closes with it, except that it stays
  launch-blocking until real recipients **and** a real channel exist (HD-PRH2-4-OPS).
  RECON-RUN-FAILED-ALERT-1 is now **required** in I-wire (§8 row 14).
- **Binding inputs:** plan §4 row 0110, §5-I, §5.0 T-1/T-2, §11 (HD-PRH2-4); ADR 0098 §5; security
  S-7 and addendum §1 (a)–(e); LF-7; the revision-2 reviews above.
- **Related:** ADR 0013, ADR 0081 §3.2, ADR 0082 (lock order; see §7.7), ADR 0094, ADR 0095
  §28.4/§28.8/§28.9, ADR 0097, ADR 0099 §6 (`app.acting_*` GUCs), ADR 0104; migrations 0084, 0105,
  0106.
- **Labels:** engineering and reversible unless marked. The `alerting.raise_failed` terminal
  fallback (§6.3) was **ACCEPTED by security** (`reviews/adr-0099-0101-security.md` Part 1) with the
  conditions applied below. No recipient, person, address, phone number or rota is named or seeded
  anywhere (HD-PRH2-4).

---

## 1. Context (verified at `cabca27`; re-checked against the reviews at `10e0471`)

**Alert sites and transactions**
- Every alert is a log line today; there is no alert table.
- The live multiple-success P1 is `auditMultipleSuccessForIntent`
  (`internal/payments/orchestrator.go:989-1024`, log at `:1018`, backstop line at `:1021`). Its
  callers are `:1062` and `:1072`, both after the dispute has been applied, inside the T10/T13d
  evidence transaction. Those transactions are reached from `receipt.go:993`, `drive.go:354` and
  `sweeper.go:513`.
- The uniform 200 is written only after `WithTenant` returns (`deposit_handlers.go:419-423`,
  `:583/:593`), and the ADR 0097 admission slot is released at the end of the handler (`:404-416`).
- `WithTenant` does not retry, and returns an error whenever `Commit` fails (`tenant_rls.go:47-64`).
- Several paths run **more than one transaction per context**: the sweeper (`:332/:370`), the
  webhook handler (`:419`, then `:483`) and `sweepTenants`.

**Reconciliation**
- Every stream commits its own transaction and logs `MISMATCH FOUND` after the commit
  (`scheduler.go:333-341,420,487,562,719`). The failure branches log `tenant run failed` at
  `:314,407,473,547,647`.
- casino_statement (`:516`) and the payment_statement match (`:683`) run under
  **`WithTenantSnapshot` (REPEATABLE READ)**.
- The findings are of two kinds (LF F8):
  - **State-type, re-detected on every run:**
    - ledger-vs-projection drift, a full-state recompute (`reconciliation.go:108`);
    - casino_consistency findings;
    - casino_statement, whose current MOCK source is all-time;
    - `pay_captured_unposted`, which is unwindowed (`payment_statement.go:941-949,989-1002`).
  - **Event-type, windowed:** the other payment-statement kinds. A lost window is not re-covered.
    Sportsbook-stream windowing has not been verified, so it is treated as event-type.
- `pay_duplicate` counts succeeded attempts only (`payment_statement.go:1087-1095`). T10/T13d leave
  the attempt `disputed` (`attempt.go:655-662`), so `pay_duplicate` does **not** back the
  multiple-success Kinds.

**Kill switch**
- The engage transaction is `payments_kill_switch_handlers.go:596-648`. Its log alert is at `:360`,
  called post-commit at `:653`.

**Handler integrity sites**
- These run on the error path, after the callback transaction has rolled back. Several of them log
  raw `err`.
- The casino-play (`casino_play_handlers.go:242-288`) and sportsbook-settlement
  (`sportsbook_settlement_handlers.go:185`) sites are reachable only through simulation-flagged
  routes (`casino_routes.go:39-43`, `sportsbook_routes.go:68-69`), like
  `payment_deposit_simulation_handlers.go:240`.

**Inputs and existing building blocks**
- `request_id` is caller-controlled free text (`middleware.go:28-33`) (SR-3).
- `db.WithPlatformService` (`platform_service.go:65`) sets `app.platform_service_id` from a closed
  allowlist (migration 0084; ADR 0081 §3.2).
- ADR 0099 introduces `app.acting_tenant_id` and `app.acting_platform_principal_id`.
- `providerref.Fingerprint` returns a 12-hex-character SHA-256 prefix (`providerref.go:94`).

## 2. Decision summary

1. **Durability.** Alerts are durable rows. Delivery is asynchronous through a provider-neutral
   `Sink`. No transaction ever performs alert I/O.
2. **Scope.** An alert is **tenant-owned** or **platform-owned**. Platform-owned alerts about a
   tenant carry `subject_tenant_id`, and that tenant can read them read-only (the ADR 0104 pattern).
   Integrity P1s are platform-owned, and **a tenant can never ack, resolve or suppress them**.
3. **Every raise site's real session can raise its Kind** (LF F2, C-102-1):
   - a platform-owned Kind raised from a tenant session has subject = that session's tenant, and
     `in_tx_raisable_by_tenant = true`;
   - `requires_subject` is a per-Kind column, enforced by trigger;
   - a detached re-raise reopens **exactly** the originating scope;
   - business code never uses the dispatcher identity.
4. **Dedup.** UNIQUE `(tenant_id, dedup_key)` NULLS NOT DISTINCT, over non-resolved alerts.
   `dedup_key` is generated and embeds the Kind and the subject tenant. Discriminators are built
   from **server-side stable ids only** (LF F6, SR-3).
5. **Payloads.** Each Kind has an attribute allowlist, enforced in Go and in the DB. Provider refs
   appear only as fingerprints. `request_id` is charset-checked. There is no PII, secret, token or
   raw error text.
6. **Routing (HD-PRH2-4).** Versioned `alert_routes` rows map (severity, scope, step) to a channel
   kind plus an **opaque recipient reference**. **No seed rows.** An alert with no route is
   `unrouted`: visible, counted and warned once (§6.1).
7. **Delivery.** Delivery and escalation state are append-only `alert_deliveries` rows. Retry uses
   bounded backoff on an injectable clock. The dispatcher runs under
   `WithPlatformService("alert_dispatcher")`: SELECT on alert tables, INSERT on deliveries, plus the
   Q1-accepted meta-Kind INSERT (§6.3).
8. **The in-transaction rule** (LF-7, addendum (a)–(e), LF F1/F4/F5, Q3):
   - Go validation happens before the savepoint and is never propagated;
   - the savepoint encloses only `Raise`;
   - a narrow SQLSTATE allowlist decides what is swallowed; transient classes propagate;
   - a per-transaction collector flushes a mandatory detached re-raise only after a nil commit, and
     after the response has been written;
   - the metric `alert_raise_failures_total{kind,phase}`;
   - a terminal fallback, `alerting.raise_failed` (security ACCEPTED, §6.3);
   - a per-Kind backstop table.
9. **REPEATABLE READ sites** (casino_statement and the payment_statement match) raise **post-commit
   only, detached**, in a fresh READ COMMITTED transaction (the orchestrator's disposition). There
   is no `ON CONFLICT` inside a snapshot (SR-5).
10. **The kill-switch engage alert is post-commit and detached only.** An alerting failure can
    never roll back the brake (LF F10, SR-4).
11. **`Raise` never writes `audit_log`** (LF F14).

## 3. Model

### 3.1 Kinds, severity, scope

`Kind` is a closed vocabulary: Go `alerting.Kinds`, mirrored in the immutable reference table
`alert_kinds`, which only a migration writes. Each Kind fixes:

| Field | Meaning |
|---|---|
| `severity` | `p1`, `p2` or `p3` |
| `scope` | `platform` or `tenant` |
| `simulation` | Whether the Kind is simulation-only |
| `requires_subject` | **New (C-102-1).** A trigger requires `subject_tenant_id IS NOT NULL` when true, and NULL when false |
| `in_tx_raisable_by_tenant` | **(LF F2)** Whether a tenant session may raise it |
| `allowed_keys` | The attribute allowlist |
| `raise_mode` | `in_tx` \| `detached` \| `post_commit`. Documentation plus the §11 table-driven test |

- **Severity:**
  - `p1` means integrity or money-correctness (CLAUDE.md: "any non-zero drift is a P1");
  - `p2` means an operational safety event or an alerting meta-warning;
  - `p3` is informational.
- **Simulation Kinds** (`simulation.` prefix) are `p3` (CHECK) and are never delivered. A trigger
  refuses any delivery row other than `suppressed_simulation` for them.
- **Scope/subject consistency rule** (enforced by a migration test that walks `alert_kinds`, LF F2):
  every `scope='platform'` Kind that is raised from a tenant session has `requires_subject = true`
  **and** `in_tx_raisable_by_tenant = true`. Only the meta-Kinds (§6.3) have
  `requires_subject = false`.

### 3.2 Tables (migration 0110)

**`alert_kinds`**
- Columns: the §3.1 fields, keyed by `kind`.
- Seeded with the §8 Kinds and the §6.3 meta-Kinds. This seeds vocabulary, not recipients.
- UPDATE, DELETE and TRUNCATE are denied.
- RLS: `ENABLE`/`FORCE` with a single `FOR SELECT USING (true)` policy, so no write policy exists.

**`alerts`**: one row per open, deduplicated condition.

| Column | Notes |
|---|---|
| `id` | |
| `tenant_id` | UUID NULL, FK. NULL means platform-owned |
| `subject_tenant_id` | UUID NULL, FK. `CHECK (subject_tenant_id IS NULL OR tenant_id IS NULL)` |
| `kind` | FK to `alert_kinds` |
| `severity`, `simulation` | Forced from `alert_kinds` by trigger |
| `discriminator` | `^[A-Za-z0-9:_.-]{1,160}$`. Built from **server-side stable ids only** (§8), never a caller-supplied value, request id or raw provider reference |
| `dedup_key` | `GENERATED ALWAYS AS (kind \|\| '\|' \|\| COALESCE(subject_tenant_id::text,'-') \|\| '\|' \|\| discriminator) STORED` |
| `attributes` | JSONB. A flat scalar object of at most 2 KiB. Keys ⊆ `allowed_keys` (trigger). A `request_id` value must match `^[A-Za-z0-9_.:-]{1,128}$` (trigger, SR-3) |
| `state` | `open` / `acked` / `resolved` |
| `acked_by`, `acked_at`, `resolved_by`, `resolved_at`, `resolve_reason_code` | Forced by trigger from the validated session actor |
| `first_seen_at`, `created_at` | |

- **Dedup index:** `UNIQUE NULLS NOT DISTINCT (tenant_id, dedup_key) WHERE state <> 'resolved'`.
  Raising after a resolve creates a new row.
- **Triggers:** scope consistency (`platform` ⇒ `tenant_id IS NULL`; `tenant` ⇒ NOT NULL) and the
  `requires_subject` check.

**`alert_occurrences`**: append-only, one row per successful `Raise`.
- Columns: `id`, `alert_id` FK, **`kind`**, `tenant_id`, `subject_tenant_id`, `raised_at`,
  `raised_by_scope`.
  - `kind`, `tenant_id` and `subject_tenant_id` are copied from the alert by trigger (SR-2). This
    lets the meta-only WITH CHECK be written directly on this table.
  - `raised_by_scope` ∈ {`tenant`, `tenant_principal`, `platform_admin`, `platform_service`} is
    **forced by trigger from the session GUCs**, never from Go (SR-1).
- The count is `count(*)` and `last_seen_at` is `max(raised_at)`. There are no counter UPDATEs.

**`alert_routes`**: versioned, effective-dated, append-only with one-way supersession.
- Columns: `id`, `scope`, `tenant_id` (NULL; always NULL in PRH-2), `severity`, `escalation_step`,
  `channel_kind CHECK IN ('log','mock')`, `recipient_ref`, `escalate_after`, `effective_from`,
  `superseded_at`, `superseded_by`, `created_by` (forced from the validated platform-admin GUC),
  `created_at`.
- `recipient_ref` is an opaque key: `CHECK (recipient_ref ~ '^[a-z0-9][a-z0-9_.:-]{0,127}$')`. It
  refuses email and phone shapes.
- **No seed rows.**

**`alert_deliveries`**: append-only; holds delivery and escalation state.

| Column | Notes |
|---|---|
| `id`, `alert_id`, `tenant_id`, `subject_tenant_id` | The last two are copied by trigger |
| `escalation_step`, `attempt_no` | |
| `event` | `claimed`, `sent`, `failed`, `unrouted`, `dead`, `suppressed_simulation` |
| `route_id`, `channel_kind` | |
| `last_error_class` | Enum: `timeout`, `unavailable`, `rejected`, `misconfigured`, `unknown`. NULL unless `failed`. Never raw error text |
| `next_attempt_at`, `next_escalation_at`, `recorded_at` | |

- `UNIQUE (alert_id, escalation_step, attempt_no, event)`.
- **`CHECK (event <> 'unrouted' OR attempt_no = 0)`** (LF F9). This makes "one `unrouted` row per
  (alert, step)" structural.
- An alert's current delivery state is its latest row.

**RLS and immutability.** All five tables are `ENABLE` + `FORCE ROW LEVEL SECURITY`. The append-only
triggers use the 0014/0016 pair (UPDATE/DELETE per row, TRUNCATE per statement). `alerts` allows
UPDATE only through the state guard (§4.2).

**Retention (LF F13).** `alert_occurrences` and `alert_deliveries` grow without bound, and no
retention or partitioning is designed here. This is registered as a follow-up (§14 item 6, with the
row text).

## 4. Scope, ownership and acknowledgement

### 4.1 RLS families (migration 0110; named)

**Common exclusion set (C-102-9, SA-2).** Every family predicate uses the `NULLIF(current_setting(
'<guc>', true), '')` form. Every family requires `app.acting_tenant_id` and
`app.acting_platform_principal_id` unset (ADR 0099), plus every scope GUC not native to that family:
`app.tenant_id`, `app.player_account_id`, `app.platform_admin_principal_id`,
`app.platform_service_id`. This extends the 0106 mixed-GUC exclusion.

| Family (policy name) | Tables | Predicate (beyond the exclusion set) | Grants |
|---|---|---|---|
| `alerts_tenant_owned` | `alerts`, `alert_occurrences`, `alert_deliveries` (SELECT) | `tenant_id = app.tenant_id` | Tenant sessions: SELECT, INSERT. **No tenant UPDATE in PRH-2** (C-102-7; no tenant-owned Kind exists) |
| `alerts_subject_tenant_read` | `alerts`, `alert_occurrences` | `tenant_id IS NULL AND subject_tenant_id = app.tenant_id` | **FOR SELECT only.** Tenants never see deliveries of platform-owned alerts |
| `alerts_subject_tenant_raise` | `alerts`, `alert_occurrences` | as above | **FOR INSERT only.** The trigger requires `in_tx_raisable_by_tenant` and forces `state='open'` with NULL ack/resolve fields |
| `alerts_platform_admin` | all five | Validated `app.platform_admin_principal_id` (a `staff_users` row with `tenant_id IS NULL`; the 0105 pattern) | SELECT all. INSERT platform-owned alerts and occurrences. State UPDATE on platform-owned alerts through the guard. INSERT and supersede `alert_routes` |
| `alerts_platform_service_dispatcher` | all five | `app.platform_service_id = 'alert_dispatcher'` | SELECT on `alert_kinds`, `alerts`, `alert_occurrences`, `alert_routes`. INSERT on `alert_deliveries`. The Q1 meta-Kind INSERT (§6.3). **No UPDATE or DELETE anywhere** |

**ON CONFLICT and RLS.** `Raise` uses `INSERT … ON CONFLICT DO NOTHING`, which needs no UPDATE
policy. Because the key embeds the subject tenant, a session can only conflict with rows its own
read family already exposes (S-7.1). This is TI-tested.

### 4.2 Acknowledgement and resolution (S-7.2, C-102-7)

- **Platform-owned alerts** are acked and resolved only by `alerts_platform_admin`, through
  platform-scope endpoints guarded by a named permission. The proposal is `alert:manage`, platform
  scope only. Its addition to `auth/permission.go` is coordinated with K1, which owns that file
  (plan §3 Rule 1).
- **There is no tenant ack endpoint in PRH-2.**
- **The state guard** (BEFORE UPDATE trigger):
  - identity, payload and dedup columns are immutable;
  - allowed transitions: `open→acked`, `open→resolved`, `acked→resolved`;
  - **the actor must resolve to a validated platform principal** (the 0105 check), and is forced
    into `acked_by`/`resolved_by`;
  - a reason code is required on resolve.
- **Every ack and resolve is audited** by the endpoint, in its own platform-admin transaction, never
  by `Raise`. When the alert has a subject, the audit row carries `audit_log.subject_tenant_id`
  (ADR 0104).
- An `acked` alert does not escalate. `resolved` stops all delivery.

### 4.3 Deliberately not built in PRH-2

- Tenant-owned Kinds, tenant-authored routes, and a tenant ack endpoint.
- The route write API is platform-admin-only and audited. SR-7 conditions it before any real
  channel exists (§6.2).

## 5. Raise API (I-core)

```go
package alerting

type Alert struct {
    Kind            Kind
    SubjectTenantID uuid.UUID            // required iff Kind.RequiresSubject
    Discriminator   string               // server-side stable ids only
    Attributes      map[string]AttrValue
}

// Scope is captured from the transaction that is being opened; it is never
// supplied by the business caller.
type Scope struct {
    Kind               ScopeKind // Tenant | TenantPrincipal | PlatformAdmin
    TenantID           uuid.UUID
    PrincipalID        uuid.UUID
    PlatformAdminID    uuid.UUID
}

// InTx opens exactly one business transaction with a collector bound to it,
// and flushes the collector only after a nil commit (LF F4).
func InTx(ctx context.Context, r ScopedRunner, fn func(ctx context.Context, tx pgx.Tx) error) (*Pending, error)
func (p *Pending) Flush(ctx context.Context) // detached re-raise in p's originating scope (§7.3)

func RaiseGuarded(ctx context.Context, tx pgx.Tx, a Alert) error // §7.2
func RaiseDetached(ctx context.Context, r ScopedRunner, a Alert) error
func Raise(ctx context.Context, tx pgx.Tx, a Alert) error        // strict; used by RaiseDetached only
```

- **Originating scope (C-102-1, SR-1).**
  - `ScopedRunner` wraps exactly one of `WithTenant`, `WithPrincipalScope` or `WithPlatformAdmin`,
    and records the resulting `Scope`.
  - `Pending` and `RaiseDetached` reopen **that same scope**. The alert's contents never influence
    the choice of scope.
  - **Go refusal:** a platform-owned Kind raised from a tenant scope whose subject differs from the
    scope's tenant is refused in Go before any SQL. It is counted, logged and **not retried**. The DB
    subject-raise policy refuses it independently.
  - **Business code never uses the dispatcher identity.** `ScopedRunner` has no service
    constructor. A static/AST test asserts that `db.ServiceAlertDispatcher` is referenced only
    inside `internal/alerting/dispatcher*.go` and `internal/alerting/fallback.go` (the `Flush`
    fallback) (Q1; security Part 1).
- **Mechanics.**
  - `INSERT … ON CONFLICT DO NOTHING RETURNING id`.
  - If no row is returned, `SELECT` the non-resolved row by `(tenant_id, dedup_key)`, then `INSERT`
    the occurrence.
  - If a concurrent resolve races the `SELECT`, retry at most twice.
- **`Raise` never writes `audit_log`** (LF F14). ADR 0104's subject-actor trigger would raise P0001
  outside the savepoint and abort T10. Audit rows for alert actions are written only by the ack and
  resolve endpoints (§4.2).
- **Go validation** covers the Kind, the attributes, the subject/scope match, the discriminator
  charset and the `request_id` charset. It runs **before** any SQL. `ProviderRefFingerprint` can
  only be built by `providerref.Fingerprint`. There is no `error` attribute type.

## 6. Delivery (I-core function; wired in I-wire)

### 6.1 Dispatcher

- **Identity.** Closed-allowlist member `db.ServiceAlertDispatcher = "alert_dispatcher"`. This is
  the ADR-level decision required by ADR 0081 §3.2.
- **Loop.**
  1. Read due work in a short read-only transaction.
  2. Call `Sink.Deliver` with **no transaction open** (a txscope guard enforces it).
  3. Record the outcome by INSERT in a fresh short transaction.
- **Due work** is any open, non-simulation alert whose latest delivery row is one of:
  - absent;
  - `failed` with `next_attempt_at ≤ now`;
  - `unrouted` (re-evaluated every pass);
  - `sent` with `next_escalation_at ≤ now` and the alert not acked.

  `now` comes from the injected `Clock`, passed as a query parameter (T-1).
- **Retry.** Bounded exponential backoff; the parameters are technical defaults in config. After
  `max_attempts`, the dispatcher records `dead`, emits an Error meta-log, increments
  `alert_dead_total{channel_kind}`, and raises `alerting.delivery_dead`.
- **Escalation.** A route with `escalate_after` sets `next_escalation_at`. If the alert is still
  `open` at that time, step `n+1` applies. With no route for step `n+1`, that step is `unrouted`.
- **Unrouted (HD-PRH2-4, LF F9).** With no effective route:
  1. `INSERT` an `unrouted` row with `attempt_no = 0` (CHECK), `ON CONFLICT DO NOTHING RETURNING`.
  2. **Only if a row was returned:** increment `alert_unrouted_total{severity}` and raise one
     occurrence of `alerting.unrouted` (p2, platform-owned, discriminator `severity:<p1|p2|p3>`).

  So K passes over M unrouted alerts produce exactly M `unrouted` rows and M meta occurrences. The
  original alert stays `open`, visible and counted until routes exist (HD-PRH2-4-OPS).
- **Meta-Kinds do not recurse (SR-6).**
  - An `alerting.*` alert that is itself unrouted gets its `unrouted` row but raises no further
    `alerting.unrouted` occurrence.
  - A meta-Kind that dies raises no `alerting.delivery_dead`.
- **Multi-instance.** The `claimed` row is the claim. Delivery is at-least-once, with sink
  idempotency key `<alert_id>:<step>:<attempt_no>`.
- **Wiring.** I-core ships the dispatcher as an unwired function. I-wire adds one line to `main.go`
  after H merges (Rule 5).

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
    Attributes      map[string]AttrValue
    RecipientRef    string
}
type RecipientResolver interface { Resolve(ctx context.Context, ref string) (Recipient, error) }
```

| Component | Label |
|---|---|
| `LogSink` | **IMPLEMENTED on merge** |
| `MockSink` (configurable failure and timeout) | **MOCK** |
| Mock resolver (`mock:` references only) | **MOCK** |
| Real channels and resolvers | **PROVIDER DEPENDENT** and out of scope (plan §8), gated by HD-PRH2-4-OPS |

**Preconditions for adding any real `channel_kind` (SR-7, C-102-8; registered with
ALERT-DELIVERY-1):**
- (i) a route change needs four-eyes, **or** raises a p2 `alerting.route_changed`;
- (ii) superseding the last effective p1 route is refused by trigger.

Neither is built in PRH-2, because only log and mock channels exist.

### 6.3 Dispatcher meta-Kind INSERT (Q1: ACCEPTED) and the terminal fallback `alerting.raise_failed` (ACCEPTED, security Part 1)

- **Meta-Kind INSERT (Q1, extended by security Part 1).** `alerts_platform_service_dispatcher` has
  **FOR INSERT** on `alerts` **and** `alert_occurrences`, with WITH CHECK:
  - `tenant_id IS NULL AND subject_tenant_id IS NULL`;
  - `kind IN ('alerting.unrouted','alerting.delivery_dead','alerting.raise_failed')`. **The list is
    exactly these three**;
  - every other scope GUC unset, including `app.acting_*`.

  On `alert_occurrences`, the same restriction applies through the copied `kind` column (SR-2). A
  trigger forces `state='open'` and `raised_by_scope='platform_service'`. There is no UPDATE, no
  DELETE and no route write.

- **Terminal fallback `alerting.raise_failed` (LF F3; ACCEPTED).**
  - **Definition:** p1, platform-owned, `requires_subject=false`. **No subject tenant**, because a
    subject would re-open SR-1.
  - **Attributes: exactly `{kind, sqlstate_class}`, enforced by a trigger** (this Kind only):
    - `kind` must be an existing `alert_kinds.kind`;
    - `sqlstate_class` must match `^[0-9A-Z]{2}$` or equal `go_validation`;
    - any other key, or a missing key, is refused;
    - **the trigger forces `discriminator := 'kind:' || attributes->>'kind'`**.

    So there is no free-text channel into this P1.
  - **When:** an alert's attempts are **exhausted** in any of these:
    - `Pending.Flush` (in-tx swallow, then detached);
    - `RaiseDetached` at the §7.3a post-commit sites, **including the REPEATABLE READ reconciliation
      sites**;
    - the §7.4 failure-path raises.

    It also fires for a Go validation failure (§7.2 step 1), with `sqlstate_class = go_validation`.
  - **Who:** only `internal/alerting/fallback.go` inserts it, under
    `WithPlatformService("alert_dispatcher")`.
    - Its constructor can build **only** `raise_failed`: an unexported function taking
      `(Kind, SQLStateClass)`.
    - The static test allowlists exactly the dispatcher files and `fallback.go`.
    - Detached re-raises of business Kinds still reopen their originating scope (§5); they never
      use this identity.
  - **Signals:**
    - the Error log line `alert_raise_fallback` **may carry `tenant_id`** (the originating scope's
      tenant) for operators;
    - the metric `alert_raise_failures_total{kind,phase}` has **no tenant label**.
  - **If the fallback itself fails:** log at Error and increment
    `alert_raise_failures_total{kind,phase="fallback"}`. **No recursion**: the fallback never raises
    about itself.
  - **Placement:** inside LF F5's post-response budget. It runs after the response is written,
    within the same detached, bounded context as `Flush` (§7.3).

## 7. The in-transaction rule (LF-7; addendum (a)–(e); Q3; LF F1, F3, F4, F5, F7, F10, F11, F12)

### 7.1 Where each raise mode is used

| Raise mode | Where |
|---|---|
| **`RaiseGuarded` (in-tx)** | Financial evidence and posting transactions (T10, T13d, any posting), and the **READ COMMITTED** reconciliation run transactions (drift, sportsbook, casino_consistency) |
| **Post-commit, detached only** (§7.3a) | The **REPEATABLE READ** reconciliation sites (casino_statement, payment_statement match), per the orchestrator's disposition (LF F7(b), SR-5); and the **kill-switch engage** (LF F10) |
| **Detached** (§7.4) | Failure-path P1s, including `reconciliation.run_failed` |

The business outcome never depends on the alert row.

### 7.2 Mechanics

1. **Validate first (LF F1).** Go validation runs **before** any SQL and before the savepoint. On
   failure:
   - **never propagate**;
   - log `alert_raise_invalid` at Error with `kind` only;
   - increment `alert_raise_failures_total{kind,phase="in_tx"}`;
   - hand the alert straight to the §6.3 fallback with `sqlstate_class = go_validation`, after the
     commit. Re-raising would fail validation again.
2. `SAVEPOINT` (pgx nested `tx.Begin`). **The savepoint encloses only `Raise`**, never any business
   statement. A mutant that widens it must be killed (Q3). If the savepoint cannot be created,
   **propagate**.
3. Run `Raise`. On error, `ROLLBACK TO SAVEPOINT`. **If that rollback fails, propagate.**
4. **Narrow swallow (a).** Swallow **only** a `*pgconn.PgError` whose SQLSTATE is class `22` or
   `23`, or exactly `42501` or `P0001`. **Everything else propagates**, including:
   - `25P02`, `40001`, `40P01`, `55P03`, `57014`;
   - classes `08` and `53`;
   - context cancellation;
   - any non-PG error.

   Propagating is safe: evidence application and reconciliation are idempotent and retried.
5. **On a swallow (c):**
   - log `alert_raise_failed` at Error with `kind` and `sqlstate_class`;
   - increment `alert_raise_failures_total{kind,phase="in_tx"}`. Labels are bounded, with no
     tenant label; `phase` ∈ {`in_tx`, `detached`, `fallback`};
   - register the alert on the transaction's `Pending`.

   A meter or logger failure is ignored.
6. **Stated plainly (Q3).** The swallowable classes are deterministic. Re-raising them in the same
   scope normally fails again. For those failures, **the real signal is the Error log plus the
   metric**, together with the §6.3 terminal fallback `alerting.raise_failed`, which gives them a
   durable marker (security Part 1 revises its earlier Q3(b) note to require one). The detached retry
   recovers transient causes only.

### 7.3 Post-commit detached retry (b); per-transaction collector (LF F4, F5, F12)

- **One collector per transaction.** `alerting.InTx(ctx, runner, fn)` binds a fresh `Pending` to
  exactly one business transaction:
  - it flushes **only if that transaction committed** (a nil return from `WithTenant` or its
    equivalent);
  - on any error it **discards** the collector.

  A swallowed alert from a rolled-back tx1 can therefore never flush after a later tx2 in the same
  context commits. An AST test asserts that no `Pending` spans two transactions.
- **When `Flush` runs (LF F5, SR-8).**
  - HTTP handlers call `Flush` **after the response has been written**, and before the handler
    returns. It still runs **inside the ADR 0097 admission hold**, which is released in the
    handler's deferred release, so the load stays bounded.
  - The 200 body and its timing are unchanged.
  - Non-HTTP callers (the sweeper and scheduler) call `Flush` right after the commit.
  - `Flush` uses a detached, bounded context (the `deniedAuditCtx` pattern,
    `payments_kill_switch_handlers.go:334`), makes at most 3 attempts with backoff on the injected
    `Clock`, and then hands off to the §6.3 fallback.
- **Mandatory.** The detached retry is **mandatory for every Kind** (§7.5).
- **Ambiguous commit (LF F12).** If `Commit` returns an error after the server actually committed,
  `Flush` is skipped. The §7.5 backstops and the metric cover this; it is not otherwise mitigated.

**§7.3a Post-commit-only sites.** At the REPEATABLE READ reconciliation sites and the kill-switch
engage, **no raise happens inside the business transaction**.
- After a nil commit, the site calls `RaiseDetached` in a fresh transaction under the originating
  scope. For RR sites that is `WithTenant`, which is READ COMMITTED, so no in-snapshot `ON CONFLICT`
  can raise 40001 (SR-5).
- **Required comment change (C-102-5).** I-wire updates the `WithTenantSnapshot` doc comment
  (`internal/db/tenant_snapshot.go:20-28`) to say that a snapshot transaction must never raise
  alerts or run `INSERT … ON CONFLICT` against rows other transactions may commit.
- **Terminal fallback.** The RR-site detached raise, like every other detached raise, falls back to
  `alerting.raise_failed` when its attempts are exhausted (§6.3; security Part 1).
- **Accepted residual (security Part 1).** A process crash between the RR-site commit and the
  post-commit raise loses that alert. No in-tx record of the intent to raise exists.
  - **This is acceptable only because** the two RR sources are **re-detected on every run**:
    casino_statement's all-time MOCK source, and payment_statement's state-type
    `pay_captured_unposted`. The next run re-raises.
  - **Revisit condition:** if an **event-type** (windowed, not re-covered) stream is ever added at
    REPEATABLE READ, or casino_statement gains a real windowed source, this residual is no longer
    acceptable. That stream then needs an in-tx durable marker (e.g. an outbox row committed with
    the run) or a security re-ruling.
  - LF F7(a) (swallowing 40001 in-snapshot) is **not adopted** (security Part 1).

### 7.4 Failure-path P1s (LF-7(2), ADR 0095 §28.8)

- **What they are:** a P1 whose business transaction **rolls back as the outcome**. That covers:
  - the handler `*_integrity_alert_*` sites;
  - `reconciliation.run_failed`.
- **How they are raised:** `RaiseDetached` in a fresh transaction of the originating scope
  (`WithTenant(t.ID)`), after the error response is written and inside the admission hold (SR-8).
- "A rolled-back event raises no alert" applies to **event** alerts only.

### 7.5 Per-Kind reconciliation backstops (d) (corrected per LF F8)

"Backstop" means a standing reconciliation check that re-surfaces the condition by itself.

| Kind | Standing backstop | Detached retry |
|---|---|---|
| `payment.multiple_success_for_intent` | **Conditional:** `pay_captured_unposted` (unwindowed, state-type), only for a provider with a wired payment-statement source (MOCK today). **`pay_duplicate` does not apply** (it counts succeeded attempts only) | **Mandatory** |
| `payment.deposit_intent_index_backstop_fired` | Same conditional `pay_captured_unposted` backstop. The disputed attempt row is durable in the same transaction | **Mandatory** |
| `reconciliation.ledger_projection_drift` | **Yes:** the drift sweep itself (state-type, re-detected every run) | **Mandatory** (uniform rule) |
| `reconciliation.casino_consistency_mismatch` | **Yes:** state-type, re-detected every run | **Mandatory** |
| `reconciliation.casino_statement_mismatch` | **Yes today:** the MOCK source is all-time, so re-detected. **This must be re-assessed** when a real, windowed source exists (§7.3a revisit condition) | **Mandatory** (post-commit detached, then fallback) |
| `reconciliation.payment_statement_mismatch` | **Partial:** `pay_captured_unposted` is state-type; the other kinds are event-type and windowed, so a lost window is not re-covered. **The §7.3a crash residual is accepted on the strength of the state-type re-detection**, and must be re-assessed if event-type kinds alone carry the signal | **Mandatory** (post-commit detached, then fallback) |
| `reconciliation.sportsbook_settlement_mismatch` | **None assumed:** windowing not verified, so treated as event-type | **Mandatory** |
| `reconciliation.run_failed` | **None.** This Kind is the signal for a lost run | **Detached primary**, bounded, then fallback |
| `payment.kill_switch_engaged` | None. The switch row and the audit row are durable | **Detached primary** (post-commit), then fallback |
| Handler integrity Kinds | Partial: `cas_*` state-type kinds, `pay_*` where a source exists, and the rejection/denial records | **Detached primary**, then fallback |
| `simulation.*` | N/A (never delivered) | Detached, best-effort |
| `alerting.*` meta-Kinds | Re-derived by the next dispatcher pass | Next pass |

Condition (b) requires the detached retry to be mandatory only for Kinds without a backstop. This
ADR makes it mandatory for **every** Kind: only some Kinds have a true, unconditional backstop, and
one uniform rule is simpler to test and to mutate. The rule is stricter than the addendum, never
looser.

### 7.6 Abort-on-failure: NO for every PRH-2 Kind (Q2 CONFIRMED; text corrected per SR-4 and LF F8)

**The reason.** An alerting failure must never roll back a reconciliation run with its mismatch
rows, a financial evidence transaction, or a safety brake. A committed run with a deferred alert is
always better than a rolled-back run.

**What still rolls back (stated honestly).** The **transient** classes that §7.2 propagates (25P02,
40001, 40P01, 55P03, 57014, 08, 53, and cancellation) still roll back the enclosing business
transaction when they surface from the alert statement at an in-tx site. That is acceptable,
because every such site is retried:
- T10/T13d are retried by provider redelivery, the sweeper or T17;
- reconciliation runs are covered by `reconciliation.run_failed`, which is raised detached from the
  failure branch, so a lost run is itself alerted. It is **required** in I-wire (RECON-RUN-FAILED-ALERT-1).

**The kill switch** raises post-commit only (§7.3a), so no alerting failure of any class can roll
back an engage.

**Future Kinds.** A future Kind may use abort-on-failure only if this ADR is amended with a reason,
and only for a business action that is (i) retried by its caller and (ii) not a safety control.

### 7.7 Lock order (LF F11). This is a note for an ADR 0082 amendment; the orchestrator owns amendment sections.

- **Alert tables are the terminal lock level.** After a `RaiseGuarded` inside a business
  transaction, the transaction takes **no further business-row lock** on a different intent,
  attempt, wallet, account or session.
- In practice, the raise is the last statement before the audit/commit tail at every in-tx site.
- The §11 `-race -count=50` test (LF test 9) checks this.

## 8. I-wire site list

Logs are retained; every raise is additional. Order: plan §5-I. Discriminators use **server-side ids
only** (LF F6, SR-3). "Subject" is the session tenant unless stated. Every platform-owned Kind below
has `requires_subject=true` and `in_tx_raisable_by_tenant=true` (LF F2).

| # | Site | Kind (sev) | Originating scope → raise mode | Discriminator | Attributes |
|---|---|---|---|---|---|
| 1 | `payments/orchestrator.go:989-1024` (log `:1018`), post-E2 | `payment.multiple_success_for_intent` (p1) | tenant (T10/T13d) → `RaiseGuarded` | `intent:<deposit_intent_id>` | `attempt_id`, `evidence_kind`, `provider_id` |
| 2 | `:1021` | `payment.deposit_intent_index_backstop_fired` (p1) | same tx → `RaiseGuarded` | `intent:<id>` | `attempt_id` |
| 3 | `reconciliation/scheduler.go:333-341` (drift) | `reconciliation.ledger_projection_drift` (p1) | tenant RC run tx (`:285`) → `RaiseGuarded`, moved inside the tx | `stream:ledger_vs_projection` | `run_id`, `mismatch_count` |
| 4 | `scheduler.go:420` | `reconciliation.sportsbook_settlement_mismatch` (p1) | tenant RC run tx → `RaiseGuarded` | `stream:sportsbook_settlement` | `run_id`, `mismatch_count`, `statement_source` |
| 5 | `scheduler.go:487` | `reconciliation.casino_consistency_mismatch` (p1) | tenant RC run tx → `RaiseGuarded` | `stream:casino_consistency` | `run_id`, `mismatch_count` |
| 6 | `scheduler.go:562` (tx at `:516`, **RR**) | `reconciliation.casino_statement_mismatch` (p1) | **post-commit detached**, `WithTenant` (RC) | `stream:casino_statement` | `run_id`, `mismatch_count`, `statement_source` |
| 7 | `scheduler.go:719` (match tx at `:683`, **RR**) | `reconciliation.payment_statement_mismatch` (p1) | **post-commit detached**, `WithTenant` (RC) | `stream:payment_statement:provider:<id>` | `run_id`, `mismatch_count`, `statement_source`, `import_id` |
| 8 | `payments_kill_switch_handlers.go:360` (called at `:653`) | `payment.kill_switch_engaged` (p2), subject = `c.target` (route-validated) | engage scope (`WithPlatformAdmin` or `WithPrincipalScope`) → **post-commit detached only** (LF F10) | `switch:<kill_switch_id>` | `provider_scope`, `operation_scope`, `reason_code`, `changed_by_scope`, `is_platform_takeover` |
| 9 | `casino_handlers.go:495,510,522,536,556` | `casino.callback_integrity.<reason>` (p1) | tenant → detached (failure path) | `provider:<provider_id>:reason:<reason>` | `provider_id`, `request_id` (charset-checked) |
| 10 | `deposit_handlers.go:451,465,518` | `payment.webhook_integrity.<reason>` (p1) | tenant → detached | `provider:<id>:reason:<reason>` | `provider_id`, `request_id` |
| 11 | `payment_deposit_simulation_handlers.go:240` | `simulation.payment.payload_mismatch` (p3, sim) | tenant → detached; never delivered | `provider:<id>` | `request_id` |
| 12 | `casino_play_handlers.go:242,252,261,276,288` (sim-gated) | `simulation.casino_play.<reason>` (p3, sim) | player's tenant (`WithTenant`) → detached; never delivered | `session:<session_id>:reason:<reason>` | `action`, `request_id` |
| 13 | `sportsbook_settlement_handlers.go:185` (sim-gated) | `simulation.sportsbook_settlement.<reason>` (p3, sim) | tenant → detached; never delivered | `bet:<bet_id>:reason:<reason>` | `reason`, `event_type`, `generation`, `bet_status`, `request_id` |
| 14 | **Required (SR-4, C-102-4; RECON-RUN-FAILED-ALERT-1):** `scheduler.go:314,407,473,547,647` (`tenant run failed`) | `reconciliation.run_failed` (p1) | tenant → detached (the run tx rolled back) | `stream:<stream>[:provider:<id>]` | `stream`, `phase`, `sqlstate_class`. **Never the error text** |

- **Bounding the alert count per entity (SR-3).** Rows 9–13 use stable server-side keys. A
  repeating provider fault is therefore **one** open alert per (tenant, provider, reason), with
  growing occurrences, not one alert per request.
- **Rows 3–7 and 14** use stable keys with the run id as an attribute. A persisting condition is one
  open alert; resolving it and re-detecting creates a new alert (LF F6).
- **The `:1509` site** dies with E2. **Row 1** is wired after E2 merges.

## 9. Migration 0110: content

1. **`alert_kinds`**: the §3.1 columns, including `requires_subject`, `in_tx_raisable_by_tenant` and
   `raise_mode`; the seed rows (§8 Kinds and the two accepted meta-Kinds, plus
   `alerting.raise_failed` (ACCEPTED, §6.3)); the immutability triggers; a SELECT-only policy.
   Also the **`raise_failed` attribute trigger**: exactly `{kind, sqlstate_class}`, `kind` an
   existing Kind, `sqlstate_class ~ '^[0-9A-Z]{2}$'` or `go_validation`, and the forced
   discriminator `'kind:'||kind`.
2. **`alerts`**: columns, CHECKs, the generated `dedup_key`, the partial UNIQUE NULLS NOT DISTINCT
   index, and these triggers: scope consistency, `requires_subject`, forced severity, the state
   guard (with validated actor), the attribute allowlist, and the `request_id` charset.
3. **`alert_occurrences`**, with the copied `kind` column and forced `raised_by_scope`;
   **`alert_routes`**; **`alert_deliveries`**, with the `attempt_no = 0` unrouted CHECK. All are
   append-only (the 0014/0016 trigger pair). Also: the `last_error_class` enum, the `recipient_ref`
   CHECK and the simulation-delivery refusal.
4. **`ENABLE` + `FORCE` RLS** on all five, with the §4.1 named families and the common exclusion
   set, including `app.acting_*` and `app.platform_service_id`.
5. **Grants:** append-only least-privilege lines in `deploy/init-app-role.sql` (Rule 4).
6. **No `alert_routes` rows.**
7. **Down:** refuse while any row exists in `alerts`, `alert_occurrences`, `alert_deliveries` or
   `alert_routes`.

## 10. Invariants

| ID | Invariant |
|---|---|
| AL-1 | Alert rows are visible only to their owner tenant, their subject tenant (read-only, without deliveries), the validated platform admin, and the dispatcher. RLS enforces this. Sessions with mixed or acting GUCs see nothing. |
| AL-2 | No tenant session can change any alert's state. |
| AL-3 | `dedup_key` embeds the Kind and the subject tenant. There is no cross-tenant conflict. Discriminators contain server-side ids only. |
| AL-4 | Attributes ⊆ the allowlist. Provider refs are fingerprints only. `request_id` is charset-checked. There is no PII, secret, token or raw error text. |
| AL-5 | No transaction is open during `Sink.Deliver`. |
| AL-6 | In a business transaction, `Raise` runs in a savepoint that encloses only `Raise`. Only the allowlisted SQLSTATEs are swallowed. |
| **AL-6a** | A Go validation failure is never propagated into the business transaction (LF F1). |
| AL-7 | A swallowed alert is re-raised only after **its own** transaction commits, in **its own** originating scope. A rolled-back transaction never flushes. |
| AL-8 | No migration seeds a route or recipient. An unrouted alert stays open and counted, with exactly one `unrouted` row per (alert, step). |
| AL-9 | Simulation Kinds are never delivered. |
| AL-10 | The dispatcher cannot UPDATE or DELETE any alert table. Its alert INSERT is limited to exactly the three meta-Kinds, with no tenant and no subject. Only `internal/alerting` (the dispatcher and `fallback.go`) references its identity, and the fallback can build only `raise_failed`. |
| AL-14 | `alerting.raise_failed` carries exactly `{kind, sqlstate_class}` with the forced discriminator. A fallback failure is logged and counted, and never recurses. |
| AL-11 | No raise runs inside a REPEATABLE READ transaction, and none runs inside the kill-switch engage transaction. |
| AL-12 | `Raise` never writes `audit_log`. |
| AL-13 | Meta-Kinds never recurse. |

## 11. Tests and mutants (T-1 injectable clock; T-2 no wall-clock assertion; T-3 local runs never labelled CI)

### I-core tests

- **IDM/CON:** N concurrent raisers of one key → 1 alert and N occurrences. After a resolve → a new
  alert.
- **TI:**
  - A's raise never conflicts with or reveals B's;
  - A cannot read B's subject alerts, or any platform deliveries;
  - A cannot raise with subject = B (refused in Go **and** by the DB with 42501).
- **RLS/AZ:**
  - there is no tenant UPDATE path;
  - mixed-GUC sessions, including tenant + `app.acting_*` and tenant + service, see and write
    nothing;
  - the dispatcher's UPDATE is refused; its INSERT of a non-meta Kind is refused **on both
    `alerts` and `alert_occurrences`** (SR-2);
  - a player session sees nothing;
  - the ack/resolve guard refuses an unvalidated actor.
- **Payload:**
  - an unknown key is refused (Go and DB);
  - a raw provider reference cannot be built as a fingerprint;
  - oversize is refused;
  - an off-enum error class is refused;
  - a `request_id` of `ops@x` or one containing a newline is refused by the DB (SR-3).
- **Routing and delivery** (clock-driven):
  - **LF test 8:** with zero routes, K passes over M alerts → exactly M `unrouted` rows and M meta
    occurrences;
  - a route added later → delivered;
  - `MockSink` failure → retries → `dead` plus `alerting.delivery_dead`;
  - escalation when not acked; no escalation when acked;
  - simulation alerts get no delivery;
  - two dispatchers → one claim;
  - no transaction is open during `Deliver`;
  - meta-Kinds don't recurse (SR-6).
- **MIG:**
  - zero routes after `up`;
  - `recipient_ref` refuses email and phone shapes;
  - `down` refuses while rows exist;
  - **LF test 3 / F2:** a table-driven walk of `alert_kinds` shows that every Kind's scope, subject
    and raisable flags let **both** the in-tx raise and `RaiseDetached` succeed from its §8
    originating session.
- **Static tests:**
  - `db.ServiceAlertDispatcher` is referenced only in `internal/alerting/dispatcher*.go` (and
    `fallback.go`), and `fallback.go` can construct only `alerting.raise_failed`;
  - no `Pending` spans two transactions.

### In-tx rule tests (the LF required tests)

| LF test | Scenario | Expected |
|---|---|---|
| 1 | In T10 via receipt, `drive.go:354` and `sweeper.go:513`: inject each swallowable class (22, 23, 42501, P0001) **and** a Go validation failure | The dispute and its audit commit; the 200 is byte-identical; no ledger transaction; SUM(D) = SUM(C); the in-tx metric increments |
| 2 | A **persistent** P0001, so the detached raise also fails | `alerting.raise_failed` persists with `{kind, sqlstate_class}` and the forced discriminator, and the 200 is unchanged in body and within its budget. A forced fallback failure logs, increments `phase="fallback"` and does not recurse |
| 4 | One context, two transactions: tx1 swallows and rolls back, tx2 commits | **No alert for tx1.** Covered for the sweeper and for webhook-plus-reversal |
| 5 | 25P02 propagates. 40001 and 40P01 raised **by the alert statement** in a READ COMMITTED transaction propagate | The idempotent re-apply gives one dispute, one audit, no posting. 55P03 and 57014 also propagate |
| 6 | Adapted to the disposition. At the RR sites, the stable key is committed concurrently | The run and its mismatch rows commit, **no raise statement executes inside the snapshot** (asserted), and the alert exists afterwards through the post-commit detached raise. The savepoint-snapshot survival question is moot under option (b) |
| 7 | A condition persisting across N runs | 1 open alert and N occurrences; resolve then the next run → a new alert |
| 9 | `-race -count=50`: two T10s on one intent plus a concurrent detached flush | One alert, no deadlock, one dispute, zero postings |
| 10 | **Kill switch (SR-4):** inject an alert failure (persistent P0001, then 40P01) in the post-commit raise | The switch stays engaged, its audit row is committed, and the response is unchanged |
| 12 | — | The existing LF-7 test set stays, unchanged |

Also required:
- an already-aborted outer transaction (25P02) is **not masked** (addendum (e));
- a failure-path P1 whose business transaction rolled back → the detached alert persists;
- a rolled-back event → no alert;
- **`reconciliation.run_failed`:** an injected run failure produces a persisted detached alert
  whose attributes contain no error text.
- **`raise_failed` hygiene (security Part 1):** a dispatcher-identity insert of `raise_failed` with
  each of the following is refused:
  - an extra attribute key;
  - a free-text or lowercase `sqlstate_class`;
  - a nonexistent `kind`;
  - a non-NULL tenant or subject;
  - a caller-supplied discriminator (it is overwritten, not honoured).
- **RR-site fallback:** a persistent failure of the casino_statement post-commit raise persists
  `raise_failed`.

### I-wire tests

- Every §8 row raises its Kind, with exactly its discriminator and attributes, from its real
  originating session.
- The multiple-success alert is durable in the same transaction as the refusal record.
- Rows 11–13 are never delivered.
- The existing log lines still fire.
- The `tenant_snapshot.go` comment is updated.

### Mutants (must be killed; LF test 11 plus earlier ones)

| Mutant | Killed by |
|---|---|
| Savepoint removed | the injected-failure tests |
| Savepoint widened to cover a business statement (Q3) | the Q3 test |
| Swallowing all errors | the propagation tests (LF test 5) |
| **Validation error propagated** | LF test 1 |
| **`Flush` called on the error path** | LF test 4 |
| **A collector shared across transactions** | LF test 4 plus the static test |
| **An incrementing `attempt_no` for `unrouted`** | LF test 8 plus the CHECK |
| **A `run:<uuid>` discriminator** | LF test 7 |
| **The fallback removed** | LF test 2 |
| The `raise_failed` attribute trigger removed | the malformed-insert test below |
| `subject_tenant_id` dropped from `dedup_key` | the TI dedup test |
| The subject-read policy without its exclusions | the mixed-GUC tests |
| The dispatcher granted UPDATE | the RLS tests |
| The simulation guard deleted | the simulation test |
| The attribute trigger deleted | the payload tests |
| One route seeded | the MIG test |
| **`RaiseDetached` choosing its scope from the Alert** | the SR-1 test (a wrong-subject alert is never retried in the subject's scope) |

## 12. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| Global `UNIQUE (dedup_key)` | The S-7.1 collision and existence leak |
| `ON CONFLICT DO UPDATE` counters | Tenants would need UPDATE on platform rows (S-7.2) |
| Delivery state as UPDATEd columns | Contradicts S-7.4 |
| `SECURITY DEFINER` raise | Grants nothing under FORCE RLS without a forbidden role change |
| Dispatcher under `WithoutTenant` | S-7.4 |
| Abort-on-failure | §7.6 |
| Denylist swallow | Condition (a) |
| Seeded routes or recipients | HD-PRH2-4 |
| Real channels now | Plan §8 |
| **`run:<uuid>` reconciliation keys** | One new P1 per tenant and stream every hour, indefinitely (LF F6) |
| **In-snapshot `ON CONFLICT` at RR sites, or swallowing 40001 there (LF F7(a))** | F7(a) would narrow addendum (a); option (b) is simpler (orchestrator disposition) |
| **A context-scoped collector** | Cross-transaction flush (LF F4) |
| **`Flush` before the response** | Delays the 200 (LF F5) |
| **Scope chosen from the Alert** | Scope laundering (SR-1) |
| **An in-tx kill-switch raise, placed last** | Transient classes could still roll back the brake; post-commit-only removes this entirely (LF F10) |

## 13. Consequences

- P1s become durable and visible. Nobody is paged until HD-PRH2-4-OPS configures routes **and** a
  real channel exists (PROVIDER DEPENDENT).
- PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking.
- Call sites use `alerting.InTx` plus `Flush`. Business outcomes and HTTP responses are unchanged.
- There is one new platform-service identity and five new tables.

## 14. Open items

1. **Resolved:** `alerting.raise_failed` was ACCEPTED by security (Part 1) and applied in §6.3.
   What remains is the §7.3a revisit condition for any future event-type stream at REPEATABLE READ.
2. **ADR 0082 amendment text** (the orchestrator writes amendment sections): "Alert tables
   (`alerts`, `alert_occurrences`) are the terminal lock level. After `alerting.RaiseGuarded` inside
   a business transaction, no further business-row lock may be taken on a different intent,
   attempt, wallet, account or session (ADR 0102 §7.7)."
3. **The `alert:manage` permission** in `auth/permission.go` needs K1 coordination (Rule 1).
4. **The SR-7 preconditions** before any real channel (§6.2), tracked under ALERT-DELIVERY-1.
5. **Re-assess the casino_statement backstop** when a real, windowed source replaces the all-time
   MOCK (§7.5).
6. **Retention (LF F13).** Proposed registry row for the orchestrator:
   > **ALERT-RETENTION-1** | devops + architect (review: security, ledger-finance) | OPEN — before
   > production launch | `alert_occurrences` and `alert_deliveries` (ADR 0102, migration 0110) are
   > append-only and grow without bound. Design a retention and partitioning policy, e.g. monthly
   > range partitions on `raised_at`/`recorded_at`, with archival of partitions older than the
   > regulatory audit horizon for **resolved** alerts only. Open alerts' occurrences and deliveries
   > are never pruned. Any deletion must go through a partition detach/archive procedure consistent
   > with the append-only triggers, never a row DELETE. The retention horizon per jurisdiction is a
   > configuration value, not a code constant.
7. **Real channels and resolvers:** PROVIDER DEPENDENT; HD-PRH2-4-OPS.
8. **Retry and backoff parameters:** technical defaults, for devops to review.

## 15. Review disposition (revision 2)

**Ledger-finance (`adr-0102-ledger-finance.md`)**

| Finding | Resolution |
|---|---|
| F1 | §7.2 step 1, AL-6a, LF test 1, mutant |
| F2 | §2(3), §3.1 flags and scope/subject rule, §8 flags, LF test 3 |
| F3 | §6.3 `alerting.raise_failed`, **ACCEPTED by security (Part 1)**, with its conditions; §7.2(6); AL-14; LF test 2 |
| F4 | §7.3 `alerting.InTx` per-transaction collector; LF test 4; static test |
| F5 | §7.3: `Flush` after the response is written |
| F6 | §8 stable `stream:` keys, `run_id` as an attribute; LF test 7 |
| F7 | Option (b), per the orchestrator: §7.3a; §8 rows 6–7; AL-11; LF test 6 adapted |
| F8 | §1, §7.5, §7.6 corrected |
| F9 | §3.2 CHECK `attempt_no = 0`; §6.1 RETURNING-gated meta occurrence; LF test 8 |
| F10 | §8 row 8 post-commit detached only; LF test 10 |
| F11 | §7.7; ADR 0082 amendment text in §14 item 2 |
| F12 | §7.3 "ambiguous commit" |
| F13 | §3.2; the registry row text in §14 item 6 |
| F14 | §2(11), §5, AL-12 |
| F15 | Noted (§8) |
| Required tests 1–12 | §11 |

**Security (`adr-0102-0104-security.md`)**

| Finding | Resolution |
|---|---|
| SR-1 / C-102-1 | §5 originating scope; `requires_subject`; forced `raised_by_scope`; static test; mutant |
| SR-2 / C-102-2 | §3.2 copied `kind` on occurrences; §6.3 WITH CHECK on both tables; RLS test |
| SR-3 / C-102-3 | §3.2 `request_id` CHECK; §8 server-side discriminators; per-entity bounding |
| SR-4 / C-102-4 | §7.6 corrected; §8 row 14 required; LF test 10 |
| SR-5 / C-102-5 | §7.3a; AL-11; `tenant_snapshot.go` comment change |
| SR-6 / C-102-6 | §6.1; AL-13 |
| SR-7 / C-102-8 | §6.2 preconditions before any real channel |
| SR-8 / C-102-8 | §7.3 and §7.4: within the admission hold |
| C-102-7 | §4.1 (no tenant UPDATE); §4.2 validated actor, `alert:manage`, no tenant ack endpoint |
| C-102-9 | §4.1 common exclusion set |
| Q1 | §6.3 accepted as specified |
| Q2 | §7.6 |
| Q3 | §7.2 steps 2, 4 and 6; the originating-scope retry; the widened-savepoint mutant |

**Orchestrator dispositions:** the RR sites are post-commit detached with stable keys (§7.3a, §8).

**Security `adr-0099-0101-security.md` Part 1 (the `raise_failed` ruling):**

| Condition | Resolution |
|---|---|
| Exactly three meta-Kinds, no tenant or subject, same restriction on occurrences, no UPDATE or DELETE | §6.3; AL-10 |
| Attributes `{kind, sqlstate_class}` by trigger; forced discriminator | §6.3; §9 item 1; AL-14 |
| No subject; the log may carry the tenant; no tenant label on the metric | §6.3 |
| Identity limited to `internal/alerting`; the fallback builds only `raise_failed` | §5; §6.3; static test |
| A fallback failure is logged with `phase="fallback"`, no recursion | §6.3; AL-14; LF test 2 |
| Within the post-response budget | §6.3; §7.3 |
| Tests: persistent P0001, malformed insert, fallback-removed mutant | §11 |
| LF F6/SR-5 confirmed; F7(a) not adopted; RR residual and revisit condition; RR fallback | §7.3a; §7.5 |

**Product-owner-proxy:** ACCEPT; no change required. The §6.3 and §7.6 rulings are closed.

**Handover/DoD:**
- **Artefacts:**
  - this ADR ACCEPTED;
  - I-core merged (0110, `internal/alerting`, the §11 I-core tests);
  - I-wire merged (the §8 rows including row 14, the dispatcher line in `main.go`, the
    `tenant_snapshot.go` comment, the §11 tests);
  - `docs/runbooks/observability-and-alerting.md` updated, with the routing matrix as a placeholder
    pending HD-PRH2-4-OPS;
  - HANDOVER rows: `LogSink` IMPLEMENTED; `MockSink` MOCK; real channels PROVIDER DEPENDENT.
- **Registry (orchestrator):**
  - ALERT-DELIVERY-1 → IMPLEMENTED (MOCK/log channels);
  - RECON-RUN-FAILED-ALERT-1 closes with I-wire;
  - PAY-P1-MULTISUCCESS-ALERT-1 stays launch-blocking;
  - add ALERT-RETENTION-1.
