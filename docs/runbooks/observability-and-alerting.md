# Observability and Alerting

Status: Stage 9 (§16). Owned by the `qa` specialist going forward, jointly
with whichever specialist owns the domain a given log event lives in.

This documents two separate things, and does not blur them:

1. **What is instrumented today** - real, running code, cited by file and
   log-event/span name, checked against HEAD as of Stage 9.
2. **A minimum alert-rule list** for a future deployment to wire up. No
   metrics backend (Prometheus, Grafana, Datadog, etc.) is deployed
   anywhere in this repository or its `deploy/` assets today - `internal/
   observability`'s `InitMetrics`/`InitTracing` register an OpenTelemetry
   SDK provider with a `stdout` exporter only (Stage 1 default,
   `internal/config`'s `OTEL_EXPORTER` env var). The rules below are
   **alert RULES to configure once a real metrics/log backend exists**,
   not a working config for one. Do not read this document as claiming a
   paging pipeline exists - it does not (`PROVIDER DEPENDENT`).

## 1. What's instrumented today

### Structured logging

`internal/observability/logging.go`'s `NewLogger` returns a JSON `slog`
logger (machine-parseable from day one, per its own doc comment).
`internal/httpserver/middleware.go`'s `requestIDMiddleware` attaches a
shared `observability.RequestState` (request id, and tenant id once
resolved) to every request's context; `observability.LoggerFromContext`
enriches any logger pulled from that context with both fields, so every
log line below can be correlated back to one HTTP request and one tenant
without threading those fields through every function call by hand.

### Request latency / error rate

- **`http_request`** (`internal/httpserver/middleware.go:73`,
  `loggingMiddleware`) - one line per request: `method`, `path`, `status`,
  `duration_ms`, plus the correlated `request_id`/`tenant_id`. This is the
  single source for both request-latency and HTTP-error-rate alerting
  today - every request, success or failure, produces exactly one of
  these.
- **`panic_recovered`** (`internal/httpserver/middleware.go:92`,
  `recoverMiddleware`) - an unhandled panic in any handler, converted to a
  clean 500 instead of crashing the process; the panic value and path are
  logged server-side only.
- OpenTelemetry HTTP server spans: `internal/httpserver/server.go`'s
  `New` wraps the whole mux in `otelhttp.NewHandler(mux, deps.ServiceName)`
  (line ~178), giving every request a trace span (method, route, status,
  duration) via the OTel SDK - exported to stdout today
  (`internal/observability/tracing.go`'s `InitTracing`), to a real
  backend once `OTEL_EXPORTER`/a collector are configured in a future
  deployment. No application code creates additional child spans yet
  (`InitTracing`/`InitMetrics`'s returned `Tracer`/`Meter` values are
  discarded in `cmd/platform-api/main.go` - the auto-instrumentation
  wrapper is the only span source today); this is disclosed here rather
  than implied by the SDK wiring alone.

### Auth failures

- `login_failed` (`internal/httpserver/auth_routes.go:290`),
  `staff_login_failed` (`internal/httpserver/staff_routes.go:193`),
  `register_failed`, `refresh_failed`, `revoke_session_failed`,
  `logout_failed` - every player/staff auth-flow handler logs its own
  named failure with the underlying error. Wrong-password/unknown-email/
  locked-out outcomes are additionally always written to the append-only
  `audit_log` table (`player.login_failed`/`player.login_blocked_lockout`
  actions, `internal/httpserver/auth_routes.go`), and are also visible in
  `http_request`'s `status` field (401/429) for the specific request.
- `internal/identity/login_attempt.go`'s `login_attempts` table (backing
  `IsLockedOut`) is the durable signal a "credential stuffing against one
  account" alert should actually query, rather than trying to reconstruct
  attempt history from log lines alone.

### Authorization (authz) failures

No dedicated log event exists for a `RequirePermission`/`RequireTenantScope`
denial (`internal/auth/permission.go`) or a bearer/principal-type
rejection (`internal/auth/middleware.go`) - both packages are under active
concurrent development by the `security` specialist this stage and were
deliberately left untouched here (see this doc's "Known gaps" section).
Today, every authz denial is still observable exactly once, generically,
via `http_request`'s `status` field (401/403) for that request - sufficient
to alert on a RATE of 401/403 responses, not to distinguish "wrong role"
from "no token" from "wrong tenant" without a per-request trace lookup.

### DB errors

No single "db_error" event exists; instead, essentially every one of this
codebase's ~130 `logger.Error("..._failed", "error", err)` call sites
(one per HTTP handler, grep `logger.Error("` across `internal/httpserver`)
logs whatever error surfaced from its own `deps.DB.WithTenant`/
`WithoutTenant` call, including a raw Postgres/connection error when
nothing more specific matched first. `readyz` (`internal/httpserver/
health.go`) proactively probes DB reachability (`HealthCheck`) and
returns 503 without its own log line - that 503 is captured by
`http_request` for `/readyz` itself, which is what an external
liveness/readiness poller already alerts on today.

### Ledger failures

- `reconciliation sweep: MISMATCH FOUND` (`internal/reconciliation/
  scheduler.go:174`) - the hourly (default `ReconciliationInterval`)
  recomputed-balance-vs-projection sweep found non-zero drift for a
  tenant/account. Per `CLAUDE.md`, any non-zero drift here is a P1.
- `reconciliation sweep: tenant run failed` / `failed to list tenants` /
  `recovered from panic` (same file) - the sweep's own operational
  failures (distinct from a drift FINDING).
- Every ledger-posting caller across `internal/httpserver` (deposit,
  casino/sportsbook bet-settlement, withdrawal, bonus) logs its own
  `..._failed` event (see DB errors above) when `ledger.Post` or a
  wrapping domain call returns an error; `internal/ledger` itself does
  not log directly (library-layer, not handler-layer - matches this
  codebase's own "log at the boundary" convention, see Provider failures
  below).

### Payment failures

- `payment_webhook_signature_invalid` / `payment_webhook_failed` /
  `payment_webhook_rejected_key_material` / `payment_webhook_tenant_lookup_
  failed` (`internal/httpserver/deposit_handlers.go:328,341` and
  neighboring lines) - the deposit/payment provider callback boundary.
- `resolve_withdrawal_provider_amount_mismatch` /
  `resolve_withdrawal_unknown_provider` and the withdrawal admin-action
  failures below cover the payout side.

### Provider / integration failures

- `kyc_provider_not_registered`, `kyc_webhook_failed`,
  `kyc_webhook_tenant_lookup_failed` (KYC vendor boundary).
- The generic `internal/providers/httpclient` client (used by adapter
  code reaching an external HTTP provider) does not log directly - it has
  no logger dependency at all, by design (a low-level library, not a
  request-handling boundary); every caller that surfaces its errors
  already logs its own named `..._failed`/`..._webhook_failed` event
  (casino/KYC/payment webhook handlers above), so the failure is not
  silently lost, just attributed to the CALLING boundary's event name
  rather than a generic "provider_http_failed" one. Documented here as a
  deliberate convention, not an oversight.

### Callback / webhook failures

- `casino_webhook_signature_invalid`, `casino_webhook_failed`,
  `casino_webhook_tenant_lookup_failed`, `casino_webhook_missing_session_
  binding` (`internal/httpserver/casino_handlers.go:355,414` and
  neighboring lines).
- `casino_webhook_integrity_alert_bet_not_found` /
  `casino_webhook_integrity_alert_provider_round_ownership_conflict`
  (same file, lines 374/389) and their `casino_play_integrity_alert_*`
  counterparts (`casino_play_handlers.go`) - these are NOT ordinary
  failures: they fire when a provider (or the mock play-simulation path)
  names a bet/round the platform's own ledger disagrees with, which
  `docs/decisions/0080-provider-integration-readiness-without-external-
  contracts.md` treats as a genuine integrity signal worth its own
  distinguishable event name, separate from an ordinary `_failed`.
- `payment_webhook_signature_invalid` / `payment_webhook_failed` (payments
  side, see above).

### Queue / event failures

- **No real message-queue/event-bus producer or consumer exists in this
  codebase today.** `internal/eventbus`'s `InMemoryBus` is an explicit
  Stage 1 STUB (its own package doc comment) with **zero callers anywhere
  in `internal/` or `cmd/`** (verified by grep as of Stage 9) - nothing
  publishes or subscribes through it yet. Logging inside an unused stub
  would be theater, not a real gap closure, so none was added; this is
  recorded as `NOT IMPLEMENTED` (no queue exists to instrument), not
  silently omitted.
- The closest REAL analogue - scheduled background sweep jobs - IS
  instrumented: `bonus cashback scheduler: tenant tick failed` / `bonus
  deposit sweep: tenant tick failed` / `bonus expiry sweep: tenant tick
  failed` (`internal/bonus`'s three sweep schedulers) and the
  reconciliation/RG-enumeration sweeps above all log their own tenant-run
  failures and panics.

### Withdrawal failures

`request_withdrawal_failed` (`internal/httpserver/withdrawal_handlers.go:
138`), `approve_withdrawal_failed` (line 644), `reject_withdrawal_failed`,
`cancel_withdrawal_failed`, `cancel_withdrawal_ownership_check_failed`,
`submit_withdrawal_failed`, `resolve_withdrawal_failed` (line 1141),
`resolve_withdrawal_provider_amount_mismatch`,
`resolve_withdrawal_unknown_provider`, `write_withdrawal_policy_failed`,
`delete_withdrawal_policy_failed` - every withdrawal state-machine
transition and admin governance action has its own named failure event.

### RG blocks / Risk blocks

Before this stage, an RG (self-exclusion/eligibility) or Risk & Limits
policy denial was recorded ONLY in the append-only `audit_log` table
(`internal/casino/orchestrator.go`'s `evaluateAndAuditEligibility`/
`evaluateAndAuditRisk`, `internal/sportsbook/orchestrator.go`'s identical
pair) - correct for compliance/forensic replay, but invisible to any
log-based alert rule without polling the audit table. This stage closes
that gap at the two real-money player-facing HTTP choke points still
being actively developed by no other concurrent workstream this session
(`internal/casino/orchestrator.go` itself is under simultaneous
financial-concurrency work by `ledger-finance` this stage and was
deliberately left untouched - see "Known gaps" below):

- **`sportsbook_bet_policy_blocked`**
  (`internal/httpserver/sportsbook_handlers.go:349`) - fires in
  `newPlaceBetHandler` exactly when `result.RejectionCategory` is
  `sportsbook.RejectionRGDenied` or `sportsbook.RejectionRiskDenied`
  (never for an ordinary commercial decline like odds-changed or
  insufficient funds - those are expected outcomes, not a control
  firing, and logging them would bury the two categories that matter in
  routine noise). Fields: `policy` (`"rg"`/`"risk"`), `reason_code`.
- **`casino_launch_policy_blocked`**
  (`internal/httpserver/casino_handlers.go:241`) - fires in
  `newLaunchCasinoGameHandler` whenever `LaunchGame` returns
  `result.Denied` with any denial code OTHER than the two jurisdiction
  codes (`casino.DenialCodeJurisdictionUnresolved`/
  `DenialCodeJurisdictionBlocked`, deliberately excluded - K3-6's
  player-facing collapse of those two is an HTTP-response-shape
  requirement only; this log is operator-only and never returned in any
  response, but jurisdiction/geo-blocking is a distinct category from
  the RG/Risk policy category this event exists to make alertable).
  Field: `reason_code`.

## 2. Minimum alert-rule list (to configure once a real backend exists)

Ordered roughly by how directly each maps to real player-money or
compliance risk on a financial platform, per `CLAUDE.md`'s financial
rules. Every rule below names the log event/table it reads and a
starting threshold - all thresholds are a reasonable Stage 9 starting
point, not a tuned SLO; whoever wires these up should revisit them
against real traffic.

1. **Ledger drift (P1, page immediately).** Any `reconciliation sweep:
   MISMATCH FOUND` log line, or any non-zero drift row from the
   reconciliation sweep's own persisted run output. Threshold: > 0
   occurrences, ever, per `CLAUDE.md`'s "any non-zero drift is a P1
   incident."
2. **Withdrawal/payment failure rate.** Rate of
   `request_withdrawal_failed`/`resolve_withdrawal_failed`/
   `payment_webhook_failed`/`payment_webhook_signature_invalid` per
   minute, alerting above a small absolute count (e.g. > 5/min) OR a
   ratio versus total withdrawal/deposit attempts (e.g. > 5%).
3. **Integrity alerts (P1/P2, page or urgent ticket).** Any
   `casino_webhook_integrity_alert_*` / `casino_play_integrity_alert_*`
   occurrence - these represent a provider or client naming a bet/round
   the ledger disagrees with, which is never expected in normal
   operation regardless of volume. Threshold: > 0.
4. **Webhook signature failures.** Rate of
   `casino_webhook_signature_invalid` / `payment_webhook_signature_
   invalid` per provider/tenant - a sudden spike suggests a
   misconfigured/rotated credential (operational) or a forgery attempt
   (security); a sustained non-zero baseline from one tenant/provider
   pair is itself worth a ticket even below a spike threshold.
5. **RG/Risk block rate.** Rate of `sportsbook_bet_policy_blocked` /
   `casino_launch_policy_blocked` by `policy`/`reason_code`, alerted on a
   SPIKE (e.g. > 3x the trailing 24h baseline for one tenant or one
   `reason_code`) rather than an absolute count - a nonzero baseline rate
   is expected (RG/self-exclusion working as intended); a sudden spike
   suggests either a policy misconfiguration (over-blocking real players)
   or a coordinated attempt to probe the boundary.
6. **Auth failure / lockout rate.** Rate of `login_failed`/
   `staff_login_failed`, and rate of `player.login_blocked_lockout` audit
   entries, per identifier and per source IP - a credential-stuffing
   pattern (many identifiers, one IP or IP range) is exactly what
   `internal/httpserver/ratelimit.go`'s own per-IP limiter bounds but does
   not itself alert on; this rule is the alerting complement to that
   control, not a replacement for it.
7. **DB/error rate.** Rate of `http_request` entries with `status >= 500`
   over total requests, per route - a generic but load-bearing signal
   given how many distinct `..._failed` events exist per handler; alert
   on ratio (e.g. > 1% 5xx over 5 minutes) rather than a single event
   name, since a new failure mode will not always have a bespoke event
   yet.
8. **Panic rate.** Any `panic_recovered` occurrence. Threshold: > 0 - a
   panic reaching `recoverMiddleware` is always a bug, never expected
   traffic.
9. **Request latency (p95/p99).** `http_request`'s `duration_ms`,
   percentiled per route - alert on a sustained p95 regression versus a
   rolling baseline (e.g. > 2x the trailing 7-day p95 for 10+ minutes),
   not a fixed absolute number, since routes have legitimately different
   costs (a catalogue read versus a financial posting).
10. **Readiness flapping.** Repeated `http_request` 503s on `/readyz`
    within a short window - the platform's own DB-health self-check
    already surfaces this; alerting on it turns an orchestrator-visible
    signal into a paged one instead of a silent restart loop.
11. **Scheduled sweep job failures.** Any `bonus cashback/deposit/expiry
    scheduler: tenant tick failed`, `reconciliation sweep: tenant run
    failed`, or `enumeration reconciliation sweep: tenant run failed` /
    `gap found` - these are the platform's only "background queue-like
    job" surface today (see §1's Queue/event failures section); a
    failure here means a whole tenant's scheduled compliance/financial
    sweep silently did not run this cycle.
12. **Payment kill-switch engaged (P1/P2, page or urgent ticket per
    tenant policy).** Any `payments_kill_switch_engaged_alert` occurrence
    (`internal/httpserver/payments_kill_switch_handlers.go`,
    `logKillSwitchEngagedAlert`, ADR 0095 §10.2/S95-C7) - a matching
    payment provider/operation has just stopped accepting new deposits or
    payouts for a tenant. Threshold: > 0, always page/notify - this is an
    operator-INITIATED containment action (most likely a live incident:
    PSP outage, suspected compromise, or a manual test), never expected
    background traffic. Fields: `tenant_id`, `provider_scope`,
    `operation_scope`, `reason_code`, `changed_by`, `changed_by_scope`,
    `request_id` - route the page to whoever owns the named
    `provider_scope` (or the whole tenant, for `*`).
    **Status (RV-PRH-I1 security review L7): the EVENT is emitted
    (IMPLEMENTED, pinned by
    `TestLogKillSwitchEngagedAlert_EmitsPinnedEventAndFields`); actual
    alert ROUTING/delivery to a paging system is NOT IMPLEMENTED - see
    "Metrics backend" in Known gaps below. This is launch-blocking per the
    security review (real alert delivery must exist before production
    launch); do not treat the log line alone as an operational alert.
    Recommended follow-up (not built): emit the equivalent event on
    RELEASE too - lifting a containment is at least as alert-worthy as
    engaging one.

## 3. Durable alerting (ADR 0102, PRH-2 I-core; `internal/alerting`; ALERT-DELIVERY-1)

**Status: IMPLEMENTED (I-core only).** This supersedes the "Metrics
backend"/"log-only" framing above for the specific event classes ADR 0102
names - it does NOT replace the log-event list in §1/§2, which stays the
day-one signal for everything not yet wired into this model. I-wire (a
later, separate workstream) connects the real business raise sites
(payment multiple-success, reconciliation mismatches, the kill switch,
handler-integrity alerts) listed in ADR 0102 §8; until I-wire merges, the
only durable alerts a real deployment will ever see are the three
`alerting.*` meta-Kinds the dispatcher itself raises.

### The model

- An **alert** (`alerts` table) is a durable, deduplicated row: one open
  row per (Kind, subject tenant, discriminator). It is **platform-owned**
  (visible to the platform admin) or, for a platform-owned Kind raised
  about a specific tenant, also visible **read-only** to that tenant as
  the alert's "subject" - a tenant can never acknowledge, resolve or
  suppress it.
- Every successful raise appends an **occurrence** (`alert_occurrences`) -
  the alert's own `count`/`last_seen_at` are `count(*)`/`max(raised_at)`
  over its occurrences, never an UPDATEd counter.
- Delivery and escalation state (`alert_deliveries`) is append-only. An
  alert's current delivery state is its latest row.
- Raising is durable and safe inside a business transaction
  (`alerting.RaiseGuarded`/`alerting.InTx`): a savepoint encloses only the
  raise, a narrow set of SQLSTATEs is swallowed, and a mandatory detached
  retry (`Pending.Flush`) runs after the response is written. If every
  attempt fails, a terminal fallback persists `alerting.raise_failed` so
  even a swallowed, exhausted raise leaves a durable, alertable trace.
  Never floating-point, never a direct balance mutation, and never
  written inside a REPEATABLE READ snapshot - see ADR 0102 §7 for the full
  in-transaction rule this package implements.

### The no-recipients-yet state (HD-PRH2-4)

**No route is ever seeded.** Migration 0108 (the ADR allocates 0110 - see
that migration's own numbering note) creates `alert_routes` completely
empty, and no code in this repository inserts a fictional recipient,
email, phone number, or on-call rota into it. This is a deliberate human
decision (`docs/decisions/0098-...md` §5, HD-PRH2-4): "No invented
people, emails, phone numbers or on-call personnel."

Consequently, **every alert raised today is `unrouted`** the first time
the dispatcher processes it: the dispatcher inserts exactly one
`unrouted` delivery row per (alert, escalation step) - re-evaluated every
pass, never duplicated (`CHECK (event <> 'unrouted' OR attempt_no = 0)`) -
and raises one occurrence of the shared, deduplicated
`alerting.unrouted` meta-alert (severity p2). The original alert stays
`open`, visible in the platform-admin's alert list, and counted
(`alert_unrouted_total{severity}`), for as long as no route exists. This
is the correct, honest state until a human operational decision
(HD-PRH2-4-OPS) configures real recipients and a real channel - it is
NOT a bug and NOT something to silence by seeding a placeholder route.

### How routes will be configured (once HD-PRH2-4-OPS is answered)

There is no route-authoring HTTP endpoint yet (PRH-2 I-wire/a later
workstream builds the platform-admin-only, audited API). Today, a route
is a plain row in `alert_routes`, insertable only by a validated
platform-admin session (`alerting_validated_platform_admin()`, migration
0108):

```sql
INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref)
VALUES ('platform', 'p1', 0, 'log', 'log:some-operator-defined-reference');
```

- `channel_kind` is `'log'` (IMPLEMENTED - `alerting.LogSink`, writes a
  structured log line) or `'mock'` (MOCK - `alerting.MockSink`, test/dev
  only, configurable failure/timeout, never wired to a real vendor). A
  real channel (email/SMS/pager/chat webhook) is **PROVIDER DEPENDENT**
  and requires an ADR amendment plus the ADR §6.2 preconditions (four-eyes
  or an audited `alerting.route_changed` alert on every route change;
  superseding the last effective p1 route is refused) before it may be
  added.
- `recipient_ref` is an **opaque** reference (`CHECK` refuses email and
  phone number shapes outright) - it names a recipient by an internal key
  the eventual real channel's adapter resolves, never a literal contact
  address stored in this table.
- Escalation (`escalate_after`) and per-severity/step routing let a P1
  page one rota at step 0 and a broader one at step 1, once real routes
  exist - none are configured today.

### Metrics and logs this stage adds

- `alert_raise_failures_total{kind,phase}` (`phase` ∈ `in_tx`, `detached`,
  `fallback`) - a raise that was swallowed, exhausted its detached retry,
  or whose own terminal fallback failed. No tenant label (ADR §6.3).
- `alert_unrouted_total{severity}`, `alert_dead_total{channel_kind}`.
- Log events: `alert_raise_invalid`, `alert_raise_failed`,
  `alert_raise_scope_mismatch`, `alert_raise_detached_exhausted`,
  `alert_raise_fallback`/`alert_raise_fallback_failed`, `alert_delivery`
  (the `LogSink`'s own delivery line), `alert_dispatcher_*` (dispatcher
  operational failures - route lookup, claim, record-outcome).

### What is stubbed / not yet wired

- **I-wire** (a separate, later workstream) connects the real business
  raise sites named in ADR 0102 §8 (payment multiple-success,
  reconciliation mismatches, the kill switch, handler-integrity alerts,
  `reconciliation.run_failed`). Until then, this model exists and is
  fully tested, but the only alerts a real deployment produces are the
  meta-Kinds.
- **The dispatcher loop is built but NOT started anywhere**
  (`alerting.RunDispatcherLoop` is exported, unwired - `cmd/platform-api/
  main.go` is owned by a different workstream). No alert is delivered in
  a running deployment until that one line is added.
- **The platform-admin ack/resolve/route-write HTTP endpoints** do not
  exist yet - only the `alert:manage` permission (platform scope) is
  registered (`internal/auth/permission.go`), so K1/I-wire can sequence
  against it. Today an admin operation is a direct, RLS-enforced SQL
  statement, not an API call.
- **Retention** (`alert_occurrences`/`alert_deliveries` grow without
  bound) has no partitioning/archival policy yet - registered as
  `ALERT-RETENTION-1`.

## Known gaps (recorded honestly, not silently deferred)

- **Casino's own RG/Risk denial paths beyond launch** (the wager/win
  callback path's `OutcomeDeclined` in `internal/casino/orchestrator.go`'s
  `postBet`) have the identical audit-only gap `casino_launch_policy_
  blocked` closes for launch, but were not touched this session:
  `internal/casino/orchestrator.go` was under live, concurrent
  financial-concurrency edits by the `ledger-finance` specialist for the
  same stage (a genuine row-lock fix, `TestStage9_ConcurrentDistinctWins
  OnLockedRound_ReleasesLockExactlyOnce`) at the time this work ran: the
  qa/ledger-finance sessions running in parallel deliberately avoided
  the same file to prevent an edit collision, per this stage's own
  explicit task split. Recommended as a fast, narrowly-scoped follow-up:
  the same `if result.Outcome == casino.OutcomeDeclined && result.
  DeclineReason is one of the RG/Risk codes` check at the `newWagerCasino
  RoundHandler`/webhook-callback boundary in `internal/httpserver`
  (handler layer, not the domain package - no collision risk there).
- **Authorization (403/401) denials** have no dedicated event, only the
  generic `http_request` status code (see §1). `internal/auth/
  middleware.go`/`permission.go` are `security`'s own territory and were
  under concurrent edit this stage (`internal/auth/middleware.go` is
  modified in this stage's working tree); adding a dedicated
  `authz_denied` event there is recommended but intentionally left to
  that specialist rather than added here.
- **`internal/eventbus`** has no real producer/consumer to instrument
  (see §1) - revisit once bonus-engine/reporting-CDC or any other
  consumer actually starts publishing through it.
- **Metrics backend.** No Prometheus/OTLP collector/Grafana exists in
  `deploy/` today. Every alert rule above assumes a future log-query or
  metrics backend capable of reading the JSON log stream and/or the OTel
  stdout trace/metric exporters' eventual real backend - this document
  defines the RULES, not a working pipeline (`PROVIDER DEPENDENT`).
