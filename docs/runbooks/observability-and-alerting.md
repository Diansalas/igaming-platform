# Observability and Alerting

Status: Stage 9 (§16). Owned by the `qa` specialist going forward, jointly
with whichever specialist owns the domain a given log event lives in.

This documents two separate things, and does not blur them:

1. **What is instrumented today** - real, running code, cited by file and
   log-event/span name, checked against HEAD as of Stage 9.
2. **A minimum alert-rule list** for a future deployment to wire up. No
   metrics backend (Prometheus, Grafana, Datadog, etc.) is DEPLOYED in any
   environment. Since A9 the platform can EXPOSE its metrics in Prometheus
   text format on a dedicated listener (`METRICS_ADDR`, section 4 and
   `alert-delivery-channels.md` section 6; off unless set), and
   `deploy/observability/prometheus.yml` plus `docker-compose.observability.yml`
   are a working local scrape example; traces and the periodic metric dump
   still go to the `stdout` exporter (`OTEL_EXPORTER`). The rules below are
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
- **`panic_recovered`** (`internal/httpserver/middleware.go`,
  `recoverMiddleware`) - an unhandled panic in any handler, converted to a
  clean 500 instead of crashing the process; logged server-side with the
  path, `panic_code` (`http_handler_panic`) and `panic_type` (the Go type
  of the panic value) only. The panic VALUE and the stack trace are never
  logged (PANIC-LOG-VALUE-1: a panic message can carry a credential or
  player PII); reproduce from the request id, route and type. The static
  guard `internal/observability/panic_value_log_guard_static_test.go`
  enforces this for every `recover()` in production code.
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

### Webhook admission metrics (ADR 0097 §8, PRH-I4-METRICS-1)

**Status: IMPLEMENTED (local; pending orchestrator merge).** `internal/observability/
webhook_admission_metrics.go`, wired from every decision point in `internal/httpserver/
webhook_admission.go`'s `admitPreAuth`/`admitVerified`/`writeDBGateUnavailable`. `security`
reviewed this and returned ACCEPT with no conditions; `code-reviewer` returned READY WITH
CONDITIONS, closed in the same branch (`docs/plans/prh2-hardening-round/reviews/j-*.md`). This
is not yet a registry closure - the orchestrator closes `PRH-I4-METRICS-1` at merge.

- `webhook_admission_decisions_total{decision,reason,provider_kind,stage}` - an OTel counter,
  incremented once per admission decision. `decision` is `admitted`/`rejected`. `reason` is one
  of `admitted`, `ip`, `preauth`, `inflight`, `db_gate`, `verified`, `domain_bulkhead`,
  `directory_unloaded`, `panic` (mirrors the existing `webhook_admission_rejected` log line's
  `tier` field, above). `provider_kind` is the webhook domain (`payments`/`casino`/`kyc`) -
  **never** a raw provider id and **never** a tenant identifier of any kind (HD-PRH-1, the
  question of whether webhook paths/slugs are tenant-confidential, is still open).
  **`stage`** is `preauth` (A2/A3/A4a/A4b) or `verified` (B1/B2) - **read `decision="admitted"`
  per-stage, never summed across both stages as "requests admitted".** A single successfully
  processed webhook callback is admitted TWICE - once at `stage="preauth"`, once at
  `stage="verified"` - so `sum(decisions_total{decision="admitted",stage="preauth"})` and
  `sum(decisions_total{decision="admitted",stage="verified"})` both approximate "successfully
  processed requests" on their own (they should track each other closely; verified admitted can
  be slightly lower than preauth admitted whenever a request is admitted at preauth but then
  fails at verified/an auth failure past this admission layer). Any rejection-RATE ratio (e.g.
  "% of preauth-stage traffic rejected") must divide by
  `admitted{stage="preauth"} + rejected{stage="preauth",...}` for that same stage, never by a
  decision total that mixes both stages together - mixing them was security review J-L2 / code
  review J-3's finding (a request admitted at both stages used to double-count `admitted` with no
  way to separate that from two distinct requests, silently understating any preauth-stage
  rejection ratio).
- `webhook_admission_inflight{provider_kind}` - an OTel `UpDownCounter` (gauge) tracking the A4a
  in-flight bulkhead's current occupancy: `+1` when a request acquires its A4a slot, `-1` when
  released (now via its own `sync.Once`, so a double release can never drive it negative - J-5).
  At rest it reads 0 for a given `provider_kind`.
- **No tenant label on either metric**, enforced by a permanent static test
  (`internal/observability/webhook_admission_closed_enum_static_test.go`, security review J-L3)
  that fails the build if any package other than `internal/observability` converts an arbitrary
  value to one of these label types - and no metrics backend is deployed (same caveat as every
  other metric in this document - see "Known gaps" below). Both instruments are created from the
  OTel *global* meter (`observability.InitMetrics` in `cmd/platform-api/main.go` installs the
  real provider; before that, or with `OTEL_EXPORTER=none`, every call is a documented no-op via
  a nil-instrument guard) - a missing or failing metrics backend, or even an instrument whose
  `Add()` panics, can never change an admission decision: every recording call happens strictly
  after the decision is made and the HTTP response already written, AND is wrapped in a recover
  of its own. `internal/observability/webhook_admission_metrics_test.go` and
  `internal/httpserver/webhook_admission_metrics_test.go` assert this directly (identical HTTP
  outcomes under a real meter, a nil instrument, a panicking instrument, and a slow instrument -
  via `observability.SetWebhookAdmissionInstrumentsForTest`, a test-only seam; the OTel global
  provider's own one-time delegate binding makes a plain second `SetMeterProvider` call
  ineffective for this purpose, code review J-1).
- **What this closes, and what it doesn't.** ADR 0097 §8 additionally specified
  `webhook_db_gate_in_use`, `webhook_limiter_keys{tier}`, `webhook_tenant_directory_size`, and
  `webhook_tenant_directory_age_seconds` gauges - those are **NOT IMPLEMENTED** (tracked as
  `PRH-I4-METRICS-2`, non-blocking; only the decisions counter and the A4a in-flight gauge were
  in this workstream's scope). See ADR 0097 §21.12 for the full implementation record.

### Payments sweeper (ADR 0095 §37, PRH-2 H)

Counters and one gauge from `internal/payments/sweeper_loop.go`, none with a tenant, provider or attempt
label: `payments_sweeper_passes_total`; `payments_sweeper_items_total{result=processed|error|panic}`;
`payments_sweeper_tenant_failures_total{phase=list|claim|panic}`;
`payments_sweeper_resolution_only_blocks_total{site=deposit_dispatch|deposit_dispatch_brand|deposit_dispatch_claim_tx_http|deposit_dispatch_claim_tx_sweeper|deposit_dispatch_claim_tx_sweeper_brand|deposit_cascade_child|deposit_cascade_child_brand|payout_reclaim|payout_resend}`;
gauge `payments_sweeper_last_pass_unix_seconds`. Log lines are prefixed `payments sweeper:` and carry tenant and
attempt ids (never provider payloads or panic values). **Proposed alert rules** (not wired; no backend and no
recipients exist, ALERT-DELIVERY-1 OPEN): sweeper stalled when `time() - payments_sweeper_last_pass_unix_seconds
> 3 x PAYMENTS_SWEEP_INTERVAL_SECONDS`; sustained `items_total{result="error"}` or any `result="panic"`; a
non-zero and growing `resolution_only_blocks_total` for longer than an agreed window (a tenant suspended with
work queued).

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

> **Rule files (OPS-READINESS-LOCAL, 2026-10-12): `deploy/observability/`.** `prometheus-alert-rules.yml` (rules over the
> metrics the platform emits), `log-event-alert-rules.yml` (signals that exist only as log lines) and
> `pending-signals.yml` (alerts that cannot be written because no signal exists: DB pool saturation, reconciliation
> staleness, drift as a metric). **NOT WIRED to any backend; ALERT-DELIVERY-1 is OPEN; nobody is paged.**
> `deploy/observability/rules_parity_test.go` fails the build if a rule references a metric, label or log event the code
> does not emit. The test found that the log events `payment_webhook_signature_invalid` and
> `casino_webhook_signature_invalid` named in item 4 below do not exist in code any more (signature rejections are visible
> as `webhook_admission_decisions_total{decision="rejected"}` and in the audit log); item 4 is stale on that point.
> The otelhttp request-duration histogram labels the route as the mux pattern path (`/readyz`), without the method.

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
13. **Webhook admission capacity rejections (P2/P3, ticket).** Rate of
    `webhook_admission_decisions_total{decision="rejected",reason=...}`
    (ADR 0097 §8, PRH-I4-METRICS-1) by `reason`/`provider_kind`/`stage` -
    a sustained non-zero rate of `stage="verified"` `verified` rejections
    for one `provider_kind` risks a vendor's own retry window (ADR 0097
    §8's own alert note); a sustained `db_gate`/`domain_bulkhead`/
    `inflight` rate suggests the pool-protection bulkheads are undersized
    for real traffic, not necessarily an attack. `directory_unloaded` at
    any rate outside a fresh deploy is itself worth paging (the admission
    layer is failing closed on every webhook route). **Ratio queries MUST
    stay within one `stage`:** e.g. a "% of preauth traffic rejected"
    alert divides `rejected{stage="preauth",...}` by
    `admitted{stage="preauth"} + rejected{stage="preauth",...}`, never by
    a decisions total that mixes both stages - see this document's
    "Webhook admission metrics" subsection above for why a single request
    legitimately records `admitted` once per stage. Threshold: no tuned
    baseline exists yet (Stage/PRH-2 addition) - start with a
    ticket-level alert on any sustained (>5 min) non-zero rate per
    `reason`/`stage`, promote `verified`/`directory_unloaded` to a page
    once real provider traffic exists.

## 3. Durable alerting (ADR 0102, PRH-2 I-core; `internal/alerting`; ALERT-DELIVERY-1)

**Status: I-core IMPLEMENTED; I-wire sites IMPLEMENTED (PRH-2 I-wire);
delivery NOT LIVE.** ADR 0102 §17 records what I-wire built: the business
raise sites below now write durable alerts, the dispatcher loop is
hardened (`alerting.RunDispatcherLoopWithConfig`), and platform-admin
ack/resolve endpoints exist. **The dispatcher IS started in
`cmd/platform-api/main.go`** (LogSink only, after the H sweeper merge) but
**no route exists**, so **no alert is delivered to anyone**: a stored
alert, a stored P1 severity or an `unrouted` delivery row is NOT a
delivery. ALERT-DELIVERY-1 stays OPEN. It closes only when real routes
exist (authored by a human, none seeded), a real channel that is neither the
log sink nor the MOCK sink is wired (PROVIDER DEPENDENT), a named human
recipient or on-call exists (HD-PRH2-4-OPS) and the ADR 0102 §17.7
preconditions are met. Wiring the dispatcher is not closure. This section supersedes the "log-only" framing of §1/§2
only for the event classes below; the log lines in §1/§2 are retained and
remain the day-one signal for everything not listed here.

### Which events now raise durable alerts

| Event | Kind (sev) | Key (discriminator) | Raised |
|---|---|---|---|
| Deposit T10/T13d park: `sync_amount_mismatch`, `provider_reference_conflict`, `invalid_provider_reference`, `poll_amount_mismatch`, `poll_reference_mismatch`, `callback_amount_asset_mismatch`, `success_for_never_sent_attempt`, `reversal_tombstone_precedes_success` | `payment.webhook_integrity` (p1) | `attempt:<attempt_id>:reason:<reason>` | in the evidence transaction (savepoint) + post-commit detached retry |
| `payments.poll_evidence_contradicts_terminal_attempt` (declined attempt, success poll contradicts) | `payment.webhook_integrity` (p1) | `attempt:<attempt_id>:reason:poll_evidence_contradicts_terminal_attempt:<poll_amount_mismatch\|poll_reference_mismatch\|provider_reference_conflict\|poll_amount_unconfirmed>` | same |
| Multiple success for one intent (T10/T13d) | `payment.multiple_success_for_intent` (p1) | `intent:<deposit_intent_id>` | same |
| INV-DEP-1 index backstop fired | `payment.deposit_intent_index_backstop_fired` (p1) | `intent:<deposit_intent_id>` | same |
| Webhook failure paths: `payload_mismatch`, `deposit_already_reversed`, `reversal_link` | `payment.webhook_integrity` (p1) | `provider:<provider_id>:reason:<reason>` | detached (the domain tx rolled back) |
| Casino callback integrity | `casino.callback_integrity` (p1) | `provider:<provider_id>:reason:<reason>` | detached |
| Ledger projection drift | `reconciliation.ledger_projection_drift` (p1) | `stream:ledger_vs_projection` | in the run tx |
| **Unlinked manual adjustment (governance breach, NOT drift)** | `reconciliation.ledger_projection_drift` (p1) | `stream:ledger_unlinked_manual_adjustment` | in the run tx |
| **Withdrawal hold invariant #12 / check A link rules (ADR 0113 check D, NOT projection drift)** | `reconciliation.ledger_projection_drift` (p1) | `stream:ledger_withdrawal_hold` | in the run tx |
| **Negative player wallet (C3, migration 0174, NOT projection drift)** | `reconciliation.ledger_projection_drift` (p1) | `stream:ledger_wallet_negative` | in the run tx |
| **Payout dispatched while its wallet was negative (C3, migration 0174, NOT projection drift)** | `reconciliation.ledger_projection_drift` (p1) | `stream:ledger_payout_while_negative` | in the run tx |
| Sportsbook / casino-consistency mismatch | `reconciliation.sportsbook_settlement_mismatch` / `reconciliation.casino_consistency_mismatch` (p1) | `stream:sportsbook_settlement` / `stream:casino_consistency` | in the run tx |
| Casino statement / payment statement mismatch (REPEATABLE READ) | `reconciliation.casino_statement_mismatch` / `reconciliation.payment_statement_mismatch` (p1) | `stream:casino_statement` / `stream:payment_statement:provider:<id>` | after the snapshot commits, detached |
| A reconciliation run transaction failed | `reconciliation.run_failed` (p1) | `stream:<stream>[:provider:<id>]` | detached |
| Kill switch engaged (a platform takeover of a tenant-engaged switch is its own alert) | `payment.kill_switch_engaged` (p2) | `switch:<kill_switch_id>` / `switch:<kill_switch_id>:takeover`; `reason_code` is a closed token or `nonconforming` | after commit, then status, headers and body bytes are flushed (the chunked body ends only when the handler returns), then the detached raise |
| A reviewer recorded a KYC-verified residence in a country the brand serves (`market.residence_into_open_market` audit action) | `kyc.residence_into_open_market` (p2) | `verification:<kyc_verification_id>`; attributes `verification_id`, `brand_id`, `request_id`, never the country | in the review transaction (`alerting.InTx` + `RaiseGuarded`), with the audit row; a non-swallowable alert failure rolls the whole review back |
| The WD-RG-1 gate (or the signer of its allow) could not decide at a payout dispatch point and failed closed (F4 residual, migration 0171; see `withdrawal-dispatch-gate.md`) | `withdrawal.payout_gate_unavailable` (p2) | `withdrawal:<id>:gate_unavailable` (T1p, every refusal) / `payout_attempt:<id>:gate_unavailable` (T2 / T12, after 3 consecutive failed ticks); attributes `stage`, `cause` | detached (the claim rolled back) |
| Deposit simulation payload mismatch | `simulation.payment.payload_mismatch` (p3, simulation) | `provider:<provider_id>` | detached; **never delivered, never paged** |

Not wired yet (listed honestly): casino-play and sportsbook-settlement
simulation alerts (ADR 0102 §8 rows 12-13, p3, never delivered anyway); payout
disputes (F-pay's surface); the terminal amount-mismatch audit
`payments.callback_amount_asset_mismatch_terminal`.

**Route and channel caution (ADR 0102 17.7 item 7).** A route whose `channel_kind` has no sink wired in
the binary (today anything other than `log`, including `mock`) is skipped on every pass with only an
`alert_dispatcher_no_sink_for_channel` log line: no delivery row, no metric, no meta-alert, and the alert
stays undelivered. Do not author such a route as a placeholder. A `log` route "delivers" only to the
application log, which is not a person.

**Routing matrix (PLACEHOLDER, pending HD-PRH2-4):** p1 = integrity or money
correctness; p2 = operational safety event or alerting meta-warning; p3 =
informational/simulation. No severity maps to any person, rota, address or
channel until a human configures `alert_routes`.

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

**No route is ever seeded.** Migration 0110 creates `alert_routes` completely
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

### How routes are configured (ADR 0102 section 18, migration 0117)

**A log or mock route is not a notification to any person.** Routing
configuration exists and is audited, but no channel that reaches a human
exists, so `alert_routing_ready` stays 0 and readiness stays NOT READY.

Routes are authored through the audited platform-admin API (permission
`alert:route_manage`, platform scope only, separate from `alert:manage`):

- `POST /v1/admin/alerting/routes` with `severity`, `escalation_step`,
  `channel_kind` (`log`, `mock`, and since A9 `email` or `webhook`), `recipient_ref` (optional while
  disabled), `escalate_after_seconds` (optional), `enabled`, `reason_code`
  (closed vocabulary: `initial_setup`, `on_call_change`,
  `escalation_change`, `correction`, `decommission`, `drill`) and, to change
  an existing route, `supersedes_id`. A route is never edited in place:
  activation or deactivation is a NEW version that supersedes the old one.
  A second current route for the same severity and step is refused (409).
- A new route starts DISABLED unless you send `enabled: true`; enabling
  requires a `recipient_ref`. `recipient_ref` is an opaque target id: no
  email, phone, address, URL or key-shaped value is accepted.
- The database refuses to enable a route for a channel kind that notifies a
  person (`email`, `webhook`) unless it supersedes a DISABLED version staged
  (`stage_for_enable: true`) by a DIFFERENT platform admin (SR-7 four-eyes, migration 0156; procedure in
  `alert-delivery-channels.md` section 3). In production, enabling a p1/p2
  route on log/mock is refused too.
- `GET /v1/admin/alerting/routes[?include_superseded=true]` lists versions;
  `GET /v1/admin/alerting/status` shows, per severity, routes configured and
  enabled, human-notification routes, `ready` and the exact missing operator
  input; `GET /v1/admin/alerts[?state=&severity=&limit=]` lists alerts with a
  derived delivery state (`pending`, `retrying`, `unrouted` + reason,
  `delivery_failed`, `suppressed`, `recorded_non_human`; `delivered` is reserved for a human
  channel and cannot occur today).
- Every successful write and every refused attempt (except a malformed alert id on
  ack/resolve from an authorized caller, a plain 400) writes an audit row
  (actor, IP, request id, reason code, before/after route ids). The
  recipient value is never audited.

Underlying table facts:

- `channel_kind` is `'log'` (IMPLEMENTED - `alerting.LogSink`, writes a
  structured log line), `'mock'` (MOCK - `alerting.MockSink`, test/dev
  only, never wired to a binary), `'email'` (IMPLEMENTED over the provider
  interface; with `email.MockProvider` it reaches no person) or `'webhook'`
  (IMPLEMENTED: one HMAC-signed HTTPS endpoint). Recipients and the URL are
  platform configuration (`ALERT_EMAIL_RECIPIENTS`, `ALERT_WEBHOOK_URL`), not route
  data. A vendor-specific channel (SMS, a pager API) is still **PROVIDER
  DEPENDENT**. Still open from ADR §6.2: an audited `alerting.route_changed`
  alert on every route change, and refusing to supersede the last effective p1
  route.
- `recipient_ref` is an **opaque** reference (`CHECK` refuses email and
  phone number shapes outright) - it names a recipient by an internal key
  the eventual real channel's adapter resolves, never a literal contact
  address stored in this table.
- Escalation (`escalate_after`) and per-severity/step routing let a P1
  page one rota at step 0 and a broader one at step 1, once real routes
  exist - none are configured today.

### Metrics and logs this stage adds

- `alert_dispatcher_passes_total{result}` (`result` ∈ `ok`, `error`, `panic`):
  one increment per dispatcher loop pass; a flat counter is the "dispatcher
  stalled" signal. No tenant label.
- `alert_raise_failures_total{kind,phase}` (`phase` ∈ `in_tx`, `detached`,
  `fallback`) - a raise that was swallowed, exhausted its detached retry,
  or whose own terminal fallback failed. No tenant label (ADR §6.3).
- `alert_unrouted_total{severity}`, `alert_dead_total{channel_kind}`,
  `alert_stale_claims_total` (a `claimed` delivery row whose lease expired
  before any outcome was recorded, and was reclaimed under a new attempt
  number - see "Stale-claim reclaim" below). No tenant label on any of
  these.
- Log events: `alert_raise_invalid`, `alert_raise_failed`,
  `alert_raise_scope_mismatch`, `alert_raise_detached_exhausted`,
  `alert_raise_fallback`/`alert_raise_fallback_failed`,
  `alert_raise_rr_deferred` (a raise deferred because the transaction was
  not READ COMMITTED), `alert_delivery` (the `LogSink`'s own delivery
  line), `alert_dispatcher_*` (dispatcher operational failures - route
  lookup, claim, record-outcome, stale-claim reclaim, a Deliver call that
  would have run with a transaction held).

### Stale-claim reclaim (security IC-2 / code review F-1)

A dispatcher process can crash, be redeployed, or OOM between claiming a
delivery attempt (`event = 'claimed'`) and recording its outcome. Without
a lease, that alert would be stranded forever - the dispatcher's own
due-work query would never look at a `claimed` row again. This is fixed:
`DispatcherConfig.ClaimLease` (technical default: 2 minutes) bounds how
long a `claimed` row is treated as in-flight; once it expires, the alert
becomes due again under a NEW attempt number, counted toward
`MaxAttempts` exactly like an ordinary failed delivery, and
`alert_stale_claims_total` increments. A permanently-wedged channel
therefore still reaches `dead` plus `alerting.delivery_dead` eventually,
rather than silently losing the alert.

### Reading a paged P1 (what the payload carries)

Every delivery carries `alert_id`, `kind`, `severity`, the subject tenant id,
the alert's **discriminator** and its allowlisted attributes. The discriminator
is the "which condition within this Kind" signal (for the T10 parks, the closed
reason after `:reason:`). It holds only server-side ids and closed reason
values, never a provider reference, player data or error text. A real channel
adapter MUST dedupe on `DedupKey = "<alert_id>:<escalation_step>"` (stable
across retry attempts); the per-attempt `IdempotencyKey` is for logs only.

### Runbook: alert dispatcher stalled

Signal: `alert_dispatcher_passes_total` has stopped increasing (or only `error`
or `panic` results increase) while open alerts exist; or open alerts with no
`alert_deliveries` row older than a few passes.
1. Is the dispatcher started? Until the `main.go` wiring lands it is NOT: that
   is the expected state, not a stall.
2. `result="panic"`: look for the `alert_dispatcher_pass_panic` log line. The
   loop survives and the next pass reclaims the stranded claim after
   `ClaimLease`. A repeating panic is a defect in a sink: fix or unroute it.
3. `result="error"`: the pass could not read due work (database or RLS
   problem): `alert_dispatcher_pass_failed` carries the error. The business
   path is unaffected (alerts are only raised, never delivered, in-tx).
4. `alert_stale_claims_total` rising: deliveries are being stranded between
   claim and outcome (sink hangs, deploys, OOM). Check the sink timeout (it is
   `ClaimLease/2`) and process restarts.
5. A rows-`dead` pile-up (`alert_dead_total`, `alerting.delivery_dead`): the
   channel is failing for a whole retry budget; fix the channel, then re-raise
   or resolve with a reason code.

### Runbook: unrouted alerts

Signal: `alert_unrouted_total{severity,reason}` > 0 or an open `alerting.unrouted`
(p2). This is the CURRENT steady state of every environment: no route is
configured. Do not seed a placeholder route. Resolve it by having a human decide
recipients/channel (HD-PRH2-4-OPS) and then configuring a route as a validated
platform admin (see "How routes are configured"). The original alerts stay
open and visible until they are acked or resolved
(`POST /v1/admin/alerts/{id}/ack|resolve`, `alert:manage`, platform admin only;
ack takes an optional `reason_code`).

By `reason` (also the `unrouted_reason` column and the list API):

- `no_route`: no current route for that severity and step. Needs a human
  routing decision (H2).
- `channel_disabled`: a current route exists but is not enabled. Enabling is a
  new route version and needs a recipient.
- `no_sink`: an ENABLED route names a channel this binary has no adapter for
  (for example `mock`, which is never wired). This is a misconfiguration, not a
  delivery: supersede the route with a wired channel kind. It used to be only a
  log line (code review N-4); it is now a counted row plus the meta-alert.

### Runbook: `alert_routing_ready == 0` (always, today)

Signal: gauge `alert_routing_ready{severity}` (p1, p2) is 0, or the Error log
`alert_routing_not_ready` (with `reason` and `missing_operator_input`). It is
computed from configuration only (never from live delivery success), and a stale
or never-evaluated cache reads 0. `GET /v1/admin/alerting/status` shows the exact
reason per severity: `no_route_configured`, `route_not_enabled`,
`channel_not_human_notification` (the route is on log/mock), `channel_not_wired`,
`evaluation_stale` (check `alert_dispatcher_passes_total` for a stall).
It stays 0 until a human-notification channel exists. `/readyz` is REPORT-ONLY:
it is not affected. The external monitor should alert on
`alert_routing_ready == 0`, an increase of `alert_dead_total`, and a flat
`alert_dispatcher_passes_total`, through a path that does not depend on this
dispatcher (its own target is operator input H8).

### Runbook: `delivery_failed` (dead) alerts

A permanent failure (`rejected`, `misconfigured`) is terminal immediately; a
retryable one is terminal after the severity attempt budget (p1 8, p2 5, p3 3
attempts). The nominal backoff caps (5 min / 1 min) are NEVER reached: backoff is
`1s << attempt`, so with the 15 s loop a vendor outage makes an alert `dead` after
roughly 3 to 4 minutes for p1, about 1 minute for p2 and about 30 seconds for p3.
A p1 that is dead at step n escalates to step n+1 when an enabled route exists.
A dead delivery is NOT re-delivered after you fix the channel (a redrive is a
closure precondition for a real channel, ADR 0102 section 18.4): list
`GET /v1/admin/alerts` and handle those alerts by hand.
`alert_delivery_attempts_total{channel_kind,result,error_class,notification}`
carries `notification=non_human` for log and mock: a `sent` there is a record,
never a person being told.

### Activation checklist (ALERT-DELIVERY-1 stays OPEN until ALL are true)

Nothing below is decided or provisioned by engineering; each needs a named human:

1. H1 channel vendor and contract (and where alert payloads are processed).
2. H2 recipients / on-call targets as the vendor's opaque refs (HD-PRH2-4-OPS).
3. H3 on-call policy (coverage, primary/secondary, ack expectations) and H4
   escalation windows (`escalate_after` per severity and step).
4. H6 provisioning of the vendor credential in the production secret store.
5. A real channel adapter that passes `alertingtest.RunChannelConformance`
   (dedupe on `DedupKey`, honours ctx, no secrets or bodies in logs, never uses
   `recipient_ref` as an address), together with four-eyes route approval and the
   security re-review (ADR 0102 section 18.3). A9 status: the webhook and email
   channels pass the conformance suite and the SR-7 four-eyes is built (migration
   0156); the security re-review and a REAL email provider / webhook receiver are
   still outstanding.
6. H5 enforce versus report for routing readiness, and H9 whether this gates the
   launch of money-moving flows.
7. H8 the external monitor's own notification target.
8. ALERT-RETENTION-1 (A9: the purge exists, migration 0157, and is OFF until
   `ALERT_RETENTION_DAYS` is set after the legal horizon decision) and a new K2
   breach paging (ADR 0102 section 17.7).

Until then: every alert is `unrouted` or recorded to a non-human channel, and no
person is notified of anything.

### Runbook: `alert_raise_failures_total{kind,phase}` rising

- `phase="in_tx"`: an in-transaction raise was swallowed (class 22/23, 42501 or
  P0001) and the business transaction committed regardless; the post-commit
  detached retry follows. Transient causes recover. A persistent cause (a
  trigger or RLS refusal) will exhaust the retry and show `phase="detached"`
  then `phase="fallback"`.
- `phase="fallback"` or a durable `alerting.raise_failed` (p1, tenant-less,
  `{kind, sqlstate_class}` only): the alert for `kind` could not be stored. The
  condition itself still has its audit row and its reconciliation backstop
  (ADR 0102 §7.5 and §17). Treat it as a P1 about alerting and investigate the
  SQLSTATE class: `23` points at a subject-tenant or constraint problem, `42`
  at a policy refusal, `P0` at a trigger refusal, `go_validation` at a code
  defect (a Kind/attribute/discriminator that fails the allowlist).

### Runbook: `reconciliation.ledger_projection_drift`

Several different findings share this Kind; the discriminator tells them apart.

- `stream:ledger_vs_projection`: true projection drift, a P1 data-integrity incident (see the financial
  incident runbook).
- `stream:ledger_unlinked_manual_adjustment`: **a governance breach, not projection drift.** A
  `manual_adjustment` posting exists that is not linked to a governed adjustment request (ADR 0100 §12).
  Balances reconcile; the control (four-eyes, closed reason code) was bypassed. Do not "rebuild the
  projection". Treat it as a compliance incident (security + ledger-finance) and find who posted it from
  the audit log.
- `stream:ledger_withdrawal_hold` (ADR 0113 check D): **a withdrawal hold invariant breach, not projection
  drift.** Mismatch kinds `withdrawal_hold_open_mismatch` (a wallet's `player_withdrawal_hold` balance rebuilt
  from `ledger_entries` differs from the sum of its non-terminal withdrawal requests) and
  `withdrawal_request_ledger_link_invalid` (a request fails check A's link rules; `actual_value` carries the
  WD03x code). The 0132 triggers make both unreachable for every session while they are enabled, so a finding
  means history, an owner `DISABLE TRIGGER`, or a defect: a P1 financial incident (ledger-finance + security);
  see operational runbook section 18. A run that could not read the tenant's rows (blind read) is NOT
  reported here: it fails and raises `reconciliation.run_failed` (`stream:ledger_vs_projection`).
- `stream:ledger_wallet_negative` (C3, migration 0174): **a negative player wallet, not projection drift.**
  Mismatch kind `player_wallet_negative_balance`, keyed by the negative `player_cash` / `player_locked_cash`
  ledger account (`actual_value` carries the balance, the ledger and projection totals, the wallet and the
  asset). The predicate is exactly the WD050 gate's (owner decision 13: credit_total < debit_total; open
  withdrawal holds are NOT netted). Typical cause: a chargeback / deposit reversal posted against funds the
  player already spent or holds in a pending withdrawal. The wallet is already blocked by the gate; the
  finding exists so the receivable is worked, not forgotten. Staff response:
  `payout-reversal-and-amount-dispute.md` section 4.1. It stops being reported on the first run after the
  balance is >= 0.
- `stream:ledger_payout_while_negative` (C3, migration 0174): **a payout was dispatched while its wallet was
  negative: the WD050 gate was raced or bypassed.** Mismatch kind `payout_dispatched_while_wallet_negative`,
  keyed by the withdrawal request (submitted or completed) whose dispatch instant (newest allowing dispatch
  decision / payout attempt send) came after the negative-making posting. Money may have left against a
  receivable: treat as a P1 financial incident (ledger-finance + payments). Staff response: section 4.1 of
  the same runbook. Both C3 findings are read-only detection: the reconciliation job never posts, never
  edits a balance and never touches the withdrawal; a blind read fails the run (`reconciliation.run_failed`).

**How to investigate (both streams).** Do NOT rely on the alert's `run_id` attribute: it is the run that
first raised the alert, and a repeat raise only adds an occurrence (which carries no attributes). Query
the **latest** reconciliation run for the tenant and stream and read its **open** mismatch rows
(`reconciliation_mismatches` for that run; for the unlinked stream, rows of kind
`ledger_unlinked_manual_adjustment`). Those rows are the current set.

**Ack-masking caveat (known, tracked).** One stable alert per (tenant, stream) means that once the alert
is open or acked, a NEW unlinked posting (or new drift) adds an occurrence only and does **not** page
again; and the unlinked finding cannot be cleared (the ledger is append-only and the link is set only by
the governed execution), so resolving the alert re-pages on the next hourly run. Acking and forgetting
therefore silences later breaches. Until the design change recorded as a precondition for closing
ALERT-DELIVERY-1 (ADR 0102 §17.7: a new K2 breach must page) is built, an operator who acks this alert
must still review the latest run's open mismatch rows on every sweep.

### What is still stubbed / not yet wired

- **The dispatcher runs but delivers nothing real until a human route is live.**
  It is wired in `cmd/platform-api/main.go` (ADR 0102 §17.6) with the log sink plus
  the email/webhook sinks IF configured (A9). With no `alert_routes` row every
  alert is `unrouted`.
- **No route exists** (HD-PRH2-4-OPS), so every alert is `unrouted` until two
  platform admins author and approve one (`alert-delivery-channels.md` section 3).
  The email channel has only a MOCK provider; a real email provider is PROVIDER
  DEPENDENT.
- **Route authoring is an audited API** (ADR 0102 section 18); enabling a
  human-notification route needs SR-7 four-eyes (built, migration 0156).
  `alert_routing_ready` stays 0 until such a route is enabled AND its sink reports
  a human notification in this process.
- **Dedicated per-reason Kinds** for the T10 parks need a migration and are
  deferred (ALERT-KINDS-DEDICATED-1).
- **Retention**: implemented (A9, migration 0157, `alerting_purge_resolved`),
  off until `ALERT_RETENTION_DAYS` is set.

## 4. A9 signals, alert kinds and their response

Implemented in A9 (metrics exposition, `deploy/observability/prometheus-alert-rules.yml`; the rules are parity-tested against the
code and against the real exposition). Labels are bounded: `stream`, `severity`, `result`, `channel_kind`, `error_class`, `table`;
there is **no tenant label** and no free text.

| Metric | Meaning | Rule |
|---|---|---|
| `db_pool_max_connections`, `db_pool_acquired_connections`, `db_pool_idle_connections` | application pool size and use | `DbPoolSaturated` (>90% for 5 m, page) |
| `db_pool_acquire_wait_seconds_total`, `db_pool_empty_acquires_total` | time and count of callers that waited for a connection | `DbPoolAcquireWaiting` (rate >0.5 for 10 m, ticket) |
| `reconciliation_last_success_unix_seconds{stream}` | OLDEST per-tenant last success of the stream; `0` = a tenant never succeeded in this process | `ReconciliationStale` (>2 h, page) |
| `reconciliation_last_sweep_unix_seconds` | end of the last COMPLETE sweep (not advanced when tenant listing failed) | `ReconciliationSweepStalled` (>2 h, page) |
| `reconciliation_payment_unreconciled{reason}` | (tenant, provider) payment_statement pairs whose latest outcome is NOT "reconciled up to now": `no_source` / `source_not_eligible` (a configured provider has no eligible statement source), `scope_unavailable` (the provider listing failed), `truncated` (the run stopped at the per-run bound), `run_failed` (fetch/ingest/match failed); no tenant label | `PaymentProviderNotReconciled` (page), `PaymentStatementBacklog` (ticket) |
| `reconciliation_mismatches_open{stream}` | mismatches reported by each tenant's latest completed run, summed; a failed run does not clear it | `ReconciliationMismatchesOpen` (>0, page) |
| `reconciliation_runs_total{stream,result}` | per-tenant stream runs: success, mismatch, failed, skipped (lock contention) | `ReconciliationRunFailures` (ticket) |
| `reconciliation_sweeps_total{result}` | sweeps: ok or listing_failed | `ReconciliationListingFailed` (page) |
| `alert_undelivered_count{severity}`, `alert_undelivered_oldest_age_seconds{severity}` | open, unacknowledged, non-simulation alerts nobody has been notified of (no delivery, unrouted, failed, dead, or sent only through a channel that reaches no person) | `AlertUndeliveredP1Old` (>15 m, page), `AlertUndeliveredP2Old` (>2 h, ticket) |
| `alert_delivery_attempts_total{channel_kind,result,error_class,notification}` | every delivery attempt; a failure is counted, never dropped | `AlertDeliveryFailing` (ticket) |
| `alert_retention_purged_total{table}`, `alert_retention_runs_total{result}`, `alert_retention_last_run_unix_seconds` | the retention purge | `AlertRetentionFailing`, `AlertRetentionStale` (ticket) |

Per-process state: the reconciliation series are held in memory and start empty after a restart (the first sweep runs at once); in a
multi-instance deployment each instance has its own series. The alert backlog is as fresh as the last completed dispatcher
pass (`alert_dispatcher_passes_total` is the liveness signal).

### Runbook: `reconciliation.sweep_failed` (P1, platform, no subject)

Raised (A9, R3-LISTING-FAILURE-LOG-ONLY-1) when the reconciliation scheduler cannot LIST tenants, or a whole sweep panics. Attributes:
`phase` (`list_active`, `list_non_active`, `panic`, `unknown`) and `sqlstate_class`; never error text. One open alert per phase
(repeats add occurrences). Meaning: nothing (or not every tenant) was reconciled in that sweep; with `list_non_active` the closed-tenant
observation (ledger drift, payment statement) is switched off while it persists. Per-tenant failures are
`reconciliation.run_failed` instead.

1. `GET /v1/admin/alerts` to read the alert; check the `ReconciliationListingFailed` / `ReconciliationSweepStalled` rules and
   `reconciliation_sweeps_total{result="listing_failed"}`.
2. The cause is almost always the database (pool exhausted, failover, statement timeout): see `DbPoolSaturated`, `/readyz`,
   `operational-runbooks.md` section 7. `sqlstate_class` `08` connection, `53` resources, `57` cancel/shutdown, `58` non-PG error.
   `phase=panic`: a bug; find the `reconciliation sweep: recovered from panic` log line (panic TYPE only) and report it.
3. Verify recovery: `reconciliation_last_sweep_unix_seconds` advances and `reconciliation_sweeps_total{result="ok"}` rises. Then
   acknowledge/resolve the alert in the back office (platform admin, reason code). Do not resolve it before a complete sweep.
4. Because the money-drift check did not run, run the per-tenant check by hand if the gap exceeded two hours
   (`operational-runbooks.md` section 3) before declaring all clear.

### Runbook: `kyc.residence_into_open_market` (P2, per tenant; F5(b))

**`review_required` is not "no effect", and the alert stays open until the verification is final.** A residence recorded on a
`review_required` review becomes evidence only when a later `approved` review keeps it; that approval writes the audit action again and
adds an occurrence to the same alert (SRF5-1). Do not acknowledge or resolve the alert while `new_status` is `review_required`; re-check
at the approval. A residence recorded into a country the brand did not serve at the time raises nothing; if the brand later opens that
country those players are admitted with no per-player alert (SRF5-3; the market opening is itself an audited governed action).

Raised when a compliance reviewer records a KYC-verified residence (`verified_residence_country` on `POST /v1/admin/kyc/verifications/{id}/review`)
in a country the brand currently serves for at least one gated operation (deposit, casino wagering, sportsbook wagering). That
residence is the evidence that admits the player to money and games, and a residence determination has NO maker-checker today (an
OWNER decision, not built): this alert is the compensating, after-the-fact control. It fires on EVERY such determination, so its
volume equals the number of residence determinations into served markets; a residence into a country the brand does not serve
raises nothing. Severity p2 is an engineering default. Attributes carry ids only (`verification_id`, `brand_id`, `request_id`); the
country is deliberately not in the alert, the audit row or the log. One open alert per verification (`verification:<id>`).
Nothing pages anyone until a human configures a route (`alert-delivery-channels.md` section 3): until then the alert is only a durable row.

Who reviews: a second person, never the recording reviewer: a different compliance officer, or the tenant admin / platform admin for
the tenant. The platform admin sees it in `GET /v1/admin/alerts?kind=kyc.residence_into_open_market`; the subject tenant sees it read-only.

What to check, per alert (acknowledge it only after these):
1. The audit row for the verification: `audit_log` where `action = 'market.residence_into_open_market'` and `target_id` = the
   `verification_id`: `actor_id` is the recording reviewer, `metadata` has `player_account_id`, `brand_id`, `new_status`. Confirm the reviewer
   is entitled to review (compliance role) and is not reviewing an account they are connected to.
2. The evidence behind the decision: the verification's documents (`GET /v1/admin/kyc/documents?player_account_id=...`) support the
   residence the reviewer recorded. The country is NOT returned by any API (by design); a support query by an authorised operator
   reads `kyc_verifications.verified_residence_country` for that row when the comparison needs it.
3. The country is in the brand's intended market set (the market policy the platform admin maintains); a residence into a market the
   brand should not serve means the market policy, not the reviewer, is wrong: raise it with the platform admin.
4. Patterns across alerts: many determinations by one reviewer in a short period, many for one brand after a policy change, or
   a determination followed at once by a deposit are escalation triggers (security / compliance lead), not routine.
5. If a determination is wrong: the correction is a new review of the verification (`rejected`, or a fresh determination), which
   withdraws or replaces the evidence immediately (`internal/kyc/market_evidence.go`); never edit `kyc_verifications` by hand.

### Runbook: `reconciliation.run_failed` (P1, per tenant and stream)

Already raised by the scheduler for every failed per-tenant stream run (RECON-RUN-FAILED-ALERT-1 was closed by I-wire; this
re-verifies it: `scheduler.go` raises it from all five stream functions, detached, discriminator `stream:<stream>[:provider:<id>]`,
no raise on graceful-shutdown cancellation). Attributes `stream`, `phase`, `sqlstate_class`. See `operational-runbooks.md` section 3
(and the `ErrWithdrawalHoldCheckBlind` note) and the `reconciliation_runs_total{result="failed"}` series.

#### `reconciliation.run_failed` phases raised by the payment_statement stream (A11)

Discriminator `stream:payment_statement:provider:<id>:<phase>` (ONE open alert per tenant, provider and phase, occurrences grow):

- `no_statement_source`: the tenant has the provider configured (a `provider_capabilities` row, or payment attempts naming it) and the
  process registry has NO statement source for it. The provider is not reconciled: no run row is written (a run row would read as clean),
  `reconciliation_payment_unreconciled{reason="no_source"}` shows it and `ReconciliationStale` stays at 0 for the stream. Register the
  provider's statement source in `paymentStatementSources()` (cmd/platform-api) or remove the stale capability row; never silence the alert.
- `source_not_eligible`: a source is registered but cannot vouch for this provider (it speaks for another provider id, or it is a MOCK source
  where mock components are not allowed). It is never fetched.
- `scope`: the tenant's provider listing failed (database).
- `cursor_anomaly` (SR11-1, SR11-10, SR11-11, F2 fix round): run rows of the pair that do not back the trusted cursor and are newer than it
  (coverage_end above it, or run_at after the cursor run's). With keys (production) a run row backs the cursor only if its RUN seal (migration
  0168) verifies and the import it names was written by this stream and source and its IMPORT seal verifies; without keys (non-production
  only) any backing import counts. The rows are IGNORED by the cursor read (the stream keeps fetching from the genuine cursor; a forged row can
  neither move nor stop it) but they are a forgery/corruption indicator: treat as a security incident (who wrote `reconciliation_runs` /
  `payment_statement_imports`?), keep the rows as evidence, and do not resolve the alert until security has reviewed. The discriminator ends
  `cursor_anomaly:n<count>:<digest>`: the digest covers the count and the ids of the unbacked rows, so an unchanged set re-occurs on ONE alert
  and a NEW forgery opens a NEW alert. Compare the audit rows `reconciliation.payment_cursor_anomaly` (count, `newest_unbacked_run_id`,
  `unbacked_digest`, `sealed_cursor`). Benign one-off causes (still investigate): keys newly configured on a stack whose history is unsealed
  (gone after the first sealed run), a statement source relabelled, a retired key id dropped from the process. Forged rows cost the read a
  linear scan (one server-side cursor read in index order, migration 0172; the count is one `count(*)`, at most 1000 rows are read for the
  digest). A flood larger than the per-read scan budget (`CursorScanBudget`, 50 000 rows) becomes `cursor_flood` (below).
  Removing forged rows is NEVER a SQL edit (the tables are append-only) and never an agent action: if their volume ever makes the read too
  slow, the remedy is an owner-authorised, audited procedure decided by security + ledger-finance (for example a migration-recorded exclusion
  of the named run ids).
- `cursor_flood` (SRF2R-1; `reconciliation.run_failed` with phase `cursor_flood`, audit `reconciliation.sweep_run_failed` phase
  `cursor_flood`): the sealed cursor read scanned `CursorScanBudget` candidate run rows without finding a verifying cursor, i.e. at least that
  many sealed-looking run rows whose seal does not verify sit above the genuine cursor. The run failed BEFORE fetching (or, if the flood
  appeared mid-sweep, its match rolled back): nothing was matched or trusted and the cursor did not move (never a zero or partial cursor).
  Every following run fails the same way while the flood stands, so the pair is NOT reconciled and M2 force-resolutions for the provider are
  refused (`evidence_scan_budget_exceeded` or `last_run_failed`). Treat as a security incident exactly like `cursor_anomaly` (the same
  forgery at volume); recovery is the owner-authorised exclusion procedure above. Raising the budget is a reviewed code change.
- `match_stalled` (SRF2-1 (c), SRF2R-2): an import of this source lies above the trusted cursor, was fetched more than 30 minutes ago, and NO
  run of this source committed after its fetch (with keys: a run whose run seal verifies). The check runs at the END of every sweep, after
  the sweep's own match, so a benign multi-instance contention loser (whose window the next committed run re-covers) is not a stall; when the
  rule still finds a stalled import the check first waits (at most 15 s, reading `pg_locks`, never taking the lock) for an in-flight match of
  another instance to commit. Every skipped match also writes an audit row `reconciliation.payment_match_skipped` (with the import id). A held
  `reconciliation:payment_statement:<tenant>` advisory lock (any database session can take one) commits no run and produces exactly this;
  the audit row `reconciliation.payment_match_stalled` then says `lock_still_held: true`: find the session holding it (`pg_locks` where
  `locktype = 'advisory'`), treat an unexpected holder as a security incident. `progress_scan_budget_exceeded: true` means the progress read
  met a run-row flood (see `cursor_flood`) and the stall was raised fail-closed. The window is not lost: every sweep re-fetches from the
  unchanged trusted cursor, and the alert stops once a match commits and the cursor passes the stalled imports.
- `truncated`: the run stopped at the per-run line bound (`RECONCILIATION_PAYMENT_STATEMENT_MAX_LINES`). The run matched only the window the
  source vouched for (`window_truncated=true` in `reconciliation_runs` and the audit row) and the next run continues from the persisted
  cursor (`reconciliation_runs.coverage_end` of the provider's last committed run). It is not a failure and not a clean claim; it clears
  when a later run completes the window. A backlog that never drains means the bound is below the provider's volume.

#### Runbook: payment_statement stuck cursor (A11)

Symptom: `reconciliation.run_failed` (phase `fetch`, for one (tenant, provider)) on EVERY run, the audit row's `error` mentions
`coverage is not contiguous with the continuation cursor` or `above the per-run bound` / `made no progress`, and
`reconciliation_payment_unreconciled{reason="run_failed"}` stays at 1 for that pair.

1. Read the pair's cursor: the `continued_from` of the pair's latest `reconciliation.sweep_run` audit row is the TRUSTED cursor the stream used
   (F2: the newest run row backed by a sealed import of this source that verifies). `SELECT max(coverage_end) FROM reconciliation_runs WHERE
   tenant_id=$1 AND stream='payment_statement' AND provider_id=$2` is only an upper bound: a value above `continued_from` means unbacked rows
   (see `cursor_anomaly`).
2. Gap or overlap error: the source returned a statement that does not start exactly at the cursor, or contains a line dated before it (a
   backdated / late-visible line, or a collect-then-page source). That is a SOURCE contract violation (`docs/plans/a11-payments-wiring-record.md`
   section 3, items 6 to 8): fix or pause the source; do not edit `reconciliation_runs` (append-only).
3. Bound error: the source returned more lines than `RECONCILIATION_PAYMENT_STATEMENT_MAX_LINES`, or an unsplittable group of lines shares one
   microsecond (`ErrPaymentStatementUnpageable`): fix the source's paging or raise the bound (<= 1000000).
4. There is no automatic skip-forward: moving a cursor past unmatched lines would be a silent false negative. A deliberate skip is a governed
   decision (owner + ledger-finance) executed as a new, audited run row, not a SQL edit.
5. Horizon error (F2 / LF-A11-2, `beyond the completeness horizon (now - settle lag)`): the source stated a CoverageEnd later than the request's
   PeriodEnd. The source must cap its coverage at PeriodEnd (and leave later lines for the next statement); if the provider's real reporting lag
   is longer than declared, raise the source's `StatementSettleLag()` (bounded to 7 days; validated at startup). Never lower the lag to make a
   run pass.
6. Order error (F2 / LF-A11-6, `ErrPaymentStatementOutOfOrder`): a truncated page was not in OccurredAt order, so the source is not paging by
   time and the page's window may be incomplete. Fix the source's provider-side paging (by OccurredAt ascending, tie groups whole).
7. A run that is SKIPPED with "horizon not past the cursor" (Info log, no failure) is normal right after a settle lag was raised; it resolves
   by itself once now - lag passes the cursor.
8. While the pair fails, M2 force-resolutions for that provider are refused (`reconciliation_not_current`, F2e): restore the stream first; do
   not work around the gate. What unblocks M2 (LFF2-3): the next successful, non-truncated, SEALED run of the provider (at most 6 h old) with
   no `reconciliation.sweep_run_failed` for the provider audited after it (a failure row dated more than 5 minutes in the future is ignored,
   so a future-dated row cannot block M2 for good). A sustained backlog (every run truncated) also keeps M2 refused until it drains.

### Runbook: undelivered alerts (`AlertUndeliveredP1Old`, `alert_undelivered_*`)

An open P1 older than 15 minutes that nobody was told about. Today (no human route) this fires whenever a P1 exists; that is the
honest ALERT-DELIVERY-1 state, not something to silence. Check `GET /v1/admin/alerting/status` for the missing operator input and
`alert-delivery-channels.md` section 3. If a route is live, look at `AlertDeliveryFailing` and `alert_dead_total` (a dead delivery is
terminal, handle by hand).

### Runbook: `DbPoolSaturated`

`db_pool_acquired_connections` near `db_pool_max_connections` with `db_pool_acquire_wait_seconds_total` rising. Look for long
transactions in `pg_stat_activity`, a slow query, or a traffic spike; the 5xx ratio and `ReadinessFailing` follow. Raising
`DATABASE_MAX_CONNS` is a capacity decision bounded by the database's `max_connections`; do not do it as a first reflex.

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
- **Metrics backend.** Metrics are exposed (A9, `METRICS_ADDR`), but no
  Prometheus/Alertmanager/Grafana is DEPLOYED anywhere: `deploy/observability/
  docker-compose.observability.yml` is a local example. The log-event rules
  still assume a future log-query backend. This document defines the RULES, not a
  working pipeline (`PROVIDER DEPENDENT`).

## PRH-2 K3 — what the force-resolution kinds emit (and do not)

The three standing kinds `pay_declared_paid_unconfirmed`, `pay_declared_not_paid_but_paid` and
`pay_declared_paid_compensated_but_paid` are produced by the `payment_statement` reconciliation stream and surfaced
through the existing `reconciliation.payment_statement_mismatch` durable alert (ADR 0102) like every other payment
mismatch. K3 adds NO alert kind and no in-transaction alert raise. **Payout disputes, including T14 (a late success
after "declare not paid"), have no direct alert** (PAY-PAYOUT-DISPUTE-ALERT-1 open); alert DELIVERY to a human is
NOT IMPLEMENTED (ALERT-DELIVERY-1 open: every alert is `unrouted`). The force-resolution routes write
`payment.manual_resolution_*` and `payment.manual_resolution_denied` audit rows; no metric or alert is derived from
them. The only new metric is `payment_m2_declared_paid_confirmed_total` (no labels). Nothing here claims that anyone
is paged.
