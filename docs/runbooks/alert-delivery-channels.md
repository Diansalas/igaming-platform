# Alert delivery channels, metrics exposure and alert retention (A9)

Status (2026-10-15): **PARTIALLY IMPLEMENTED.** The email channel (over the `internal/email` provider interface), one
HMAC-signed webhook channel, SR-7 four-eyes route approval, the Prometheus `/metrics` listener, the new signals and
rules, the `reconciliation.sweep_failed` alert and the alert-history retention purge are implemented and tested.
**ALERT-DELIVERY-1 is still OPEN**, and this document says exactly what remains:

| Piece | Label |
|---|---|
| Webhook channel (`alerting.WebhookSink`) | IMPLEMENTED. Needs a human-staged and second-admin-enabled route, a receiver URL and a secret to deliver anything |
| Email channel (`alerting.EmailSink`) | IMPLEMENTED over the provider interface; the only provider is `email.MockProvider` (**MOCK**), so an email alert reaches **no person**; a real SMTP/API adapter is **PROVIDER DEPENDENT**. Production refuses the MOCK provider |
| Route authoring and four-eyes (SR-7) | IMPLEMENTED (migration 0156); no route is seeded: a human must author one |
| Prometheus exposition (`METRICS_ADDR`) | IMPLEMENTED |
| Prometheus / Alertmanager deployment, receiver, on-call rota | **NOT IMPLEMENTED** / human decision (HD-PRH2-4-OPS); `deploy/observability/prometheus.yml` and `docker-compose.observability.yml` are an EXAMPLE |
| Alert retention purge | IMPLEMENTED (migration 0157), **off by default** until a horizon is configured (a legal/compliance decision) |

## 1. Interim recipient (owner decision 2026-10-15)

Delivery decision: **email plus one webhook**. The owner is the **interim** recipient until a rota exists:

| Item | Interim value |
|---|---|
| Alert email recipient | `diego.salas806@gmail.com` |
| Alert webhook | none chosen yet (a receiver URL and signing secret are an owner input) |

The address lives **only in configuration**, never in code or in a route row:

```sh
# platform-api environment (staging/production: from the deployment's secret/config system, not a committed file)
ALERT_EMAIL_RECIPIENTS=diego.salas806@gmail.com          # comma-separated bare addresses, at most 10
ALERT_WEBHOOK_URL=https://<receiver>/<path>              # optional; https only
ALERT_WEBHOOK_SECRET_REF=awssm://arn:aws:secretsmanager:<region>:<account>:secret:<name>?versionId=<id>
```

Replacing the interim recipient with a rota is a configuration change plus a restart; no migration, no code change.

**Honest status of the interim recipient:** with only `email.MockProvider` available (no real email vendor,
`docs/decisions/0030` section 4), setting `ALERT_EMAIL_RECIPIENTS` in development records what would have been sent and
**nobody is emailed**; `GET /v1/admin/alerting/status` reports the email channel as not a human notification. In production the
process refuses to start with the MOCK provider (MOCK-ADAPTER-PROD-1). The owner receives alerts only once a real email
provider adapter exists (PROVIDER DEPENDENT) or the webhook points at a receiver that notifies the owner.

## 2. Configuration reference

| Variable | Meaning | Default |
|---|---|---|
| `METRICS_ADDR` | `host:port` of the dedicated Prometheus listener | unset = no listener |
| `METRICS_ALLOW_NON_LOOPBACK` | `true` acknowledges a non-loopback bind in production | false |
| `ALERT_EMAIL_RECIPIENTS` | email channel recipients | none (no email sink is built) |
| `ALERT_WEBHOOK_URL` | the single webhook endpoint | none (no webhook sink is built) |
| `ALERT_WEBHOOK_SECRET_REF` | secret-store reference (`awssm://` or `devfile://`) of the HMAC signing secret, at least 32 bytes | required with the URL |
| `ALERT_RETENTION_DAYS` | resolved-alert retention window in days (minimum 7) | unset = no purge job |
| `ALERT_RETENTION_INTERVAL_HOURS` | purge cadence | 24 |

Every one is validated at startup (`internal/config/observability.go`); a bad value or a configured-but-unbuildable channel
is a startup error, never a silent "unrouted". A half configuration (URL without a secret reference, a reference without a
URL) is refused. The `recipient_ref` of a route is only an opaque label (for example `interim-owner`): it is never an
address, URL or credential and the channels never read it as one.

## 3. Making a human route live (SR-7 four-eyes, two platform admins)

A route is configuration; the channel configuration above says WHERE, the route says WHEN (severity and step). The database
refuses to enable a route for `email` or `webhook` alone (`AR001`). Procedure, with two different platform admins A and B:

1. **A stages** the route DISABLED. `POST /v1/admin/alerting/routes` with `{"severity":"p1","escalation_step":0,
   "channel_kind":"email","recipient_ref":"interim-owner","enabled":false,"reason_code":"initial_setup"}`.
   (If a log/mock route already exists for that severity and step, add `supersedes_id` of it: while the disabled version is
   current, alerts for that step read `unrouted/channel_disabled`.)
2. **B enables** it: the same body with `"enabled":true`, `"supersedes_id":"<A's route id>"` and a `reason_code`
   (`on_call_change` for a change, `initial_setup` for the first). The database checks that the staged version exists, is
   current and disabled, is identical in severity, step, channel, recipient and escalation, was created by a **different**
   validated admin, is at most 7 days old, and has not been consumed before. The audit row records
   `four_eyes_staged_route_id` and `four_eyes_staged_by`.
3. Check `GET /v1/admin/alerting/status`: the severity is `ready` only if the sink is wired in this process **and** reports
   `HumanNotification` (a MOCK-provider email sink and a development-posture webhook sink do not).

Production also refuses an enabled p1/p2 route on a channel that notifies nobody (log, mock).

## 4. Webhook contract (for the receiver)

`POST <ALERT_WEBHOOK_URL>`, `Content-Type: application/json`, headers:

| Header | Meaning |
|---|---|
| `X-Alert-Timestamp` | unix seconds when signed |
| `X-Alert-Signature` | `v1=` + hex `HMAC-SHA256(secret, timestamp + "." + raw body)` |
| `Idempotency-Key` | `<alert_id>:<escalation_step>`, **stable across retries**: dedupe on it |
| `X-Alert-Id` | the alert id |

Body: `{"schema":"igaming.alert.v1","alert_id","dedup_key","kind","severity","discriminator","subject_tenant_id"?,"sent_at"}`.
Identifiers and closed codes only: **no attribute values and no PII** (some kinds record operator-entered text, so no attribute
is forwarded). Receiver rules: verify the signature in constant time over the raw body; reject a timestamp more than 5 minutes
from your clock (replay); treat a repeated `Idempotency-Key` as already handled; answer 2xx fast. Delivery is
**at-least-once**: the dispatcher retries a retryable failure with backoff (p1 8 attempts, p2 5, p3 3) and records every attempt in
`alert_deliveries`; a duplicate is possible, a silent drop is not (a failure ends as `dead`, visible and counted).

Response classes: `2xx` sent; `408/425/429/5xx` and transport errors/timeouts retried; other `4xx` = `rejected` (permanent);
`3xx` = `misconfigured` (redirects are never followed); a destination that resolves to a non-public address =
`misconfigured` (see 5). Rotating the secret: references are version-pinned, so publish the new version in the secret
store, point `ALERT_WEBHOOK_SECRET_REF` at it and restart (the sink caches the fetched secret for 5 minutes, so even a
store-side change to the same pinned version takes effect within 5 minutes). The receiver should accept both secrets during the
overlap.

## 5. Webhook egress protection (what a security review should probe)

The URL is configuration and the only destination; it is never built from an alert, a route or `recipient_ref`.
`internal/egressguard`: (1) static URL validation (https only; loopback http only under an explicit `APP_ENV=development`;
no userinfo, fragment, numeric-obfuscated or private IP literal, `localhost`/`.internal`/`.local` names); (2) a dial-time check
of the actually connected address after DNS (loopback, RFC 1918, link-local including 169.254.169.254, CGNAT, ULA, multicast,
reserved, NAT64/6to4/Teredo are refused for every connection, defeating DNS rebinding); (3) no redirects; (4) no environment
proxy; (5) bounded dial 3 s, TLS 3 s, response header 5 s, total 10 s, TLS 1.2 minimum; (6) response body capped and
discarded; (7) only a failure CLASS is logged, never the URL (it can carry a token) or the secret. The signing secret is
fetched from the secret store by reference (minimum 32 bytes, otherwise `misconfigured`, no request sent).

## 6. Metrics exposure (`METRICS_ADDR`)

The OpenTelemetry meter provider also feeds a Prometheus-format handler served by `internal/observability.NewMetricsServer`
on its **own** `http.Server`: `GET`/`HEAD /metrics` only, every other path 404, header/read/write/idle timeouts set, a
private registry (plus Go and process collectors), no authentication. It is **never** on the public API mux (a static test
pins that). Network exposure:

- Default (unset): nothing listens.
- Development: `METRICS_ADDR=0.0.0.0:9464` (or loopback) is fine.
- Staging/production: bind **`127.0.0.1:9464`** and scrape through a sidecar/agent on the same host/task, or bind a private
  interface and allow only the collector's security group/network policy to that port. `config.Load` refuses a non-loopback
  bind when `APP_ENV` is production or unset unless `METRICS_ALLOW_NON_LOOPBACK=true` acknowledges the network control. The
  port must differ from `HTTP_ADDR`'s. Never put it behind the public load balancer.
- The exposition carries series names, closed label values and counts only. It contains no tenant id, no secret and no
  request data; labels are bounded (stream, severity, result, channel kind...). Process/Go collectors reveal the binary's
  runtime facts, which is why the endpoint is not public.

A local scrape-and-evaluate example: `deploy/observability/prometheus.yml` and `docker-compose.observability.yml` (loopback
UI only, no Alertmanager, no credentials, rules file mounted read-only). Metric names: see `observability-and-alerting.md`
section 4 and `deploy/observability/prometheus-alert-rules.yml` (parity-tested against the code).

## 7. Alert retention (ALERT-RETENTION-1)

`alert_occurrences`, `alert_deliveries` and `alerts` are append-only. Migration 0157 adds the one sanctioned deletion path,
`alerting_purge_resolved(window, batch)`:

- only **resolved** alerts older than the window (minimum 7 days, enforced by the database too) and their occurrences and
  deliveries; **open and acknowledged alerts are never pruned**, whatever their age;
- callable only by the `alert_dispatcher` platform-service session; the runtime role has no `DELETE` on any alert table, the
  new DELETE RLS policies need the dispatcher identity AND a transaction-local flag, and the delete triggers refuse anything
  outside the function (flag set, session user is the table owner, row eligible); no SELECT policy was added anywhere;
- every run (including an empty one) appends a row to `alert_retention_runs` (counts, window, cutoff, capped flag: no alert
  content). It is readable by platform admins; it is itself append-only;
- `ALERT_RETENTION_DAYS` unset means **no purge job** and a startup warning: the horizon is a legal/compliance decision per
  jurisdiction, not a code constant. Set it only after that decision. Large backlogs drain over several ticks (5000 alerts per
  call, at most 20 calls per tick);
- metrics: `alert_retention_purged_total{table}`, `alert_retention_runs_total{result}`, `alert_retention_last_run_unix_seconds`.

Residual: occurrences of a long-lived OPEN alert (an alert storm on one dedup key) are retained until it is resolved.
Rollback of 0157 is refused while `alert_retention_runs` has rows (it is the audit of what was deleted); purged history cannot be
restored by any down migration, only from a backup.

## 8. Quick checks

```sh
curl -s http://127.0.0.1:9464/metrics | grep -E '^(db_pool|reconciliation|alert_)'   # signals present
# platform admin API: GET /v1/admin/alerting/status   (ready per severity and the exact missing input)
#                     GET /v1/admin/alerts            (derived delivery state; delivered only via a human channel)
```
