# A9 observability: independent security review (live)

- Branch `a9-observability`, commit `1c1deff` (base `f9a31e7`).
- Reviewer: `security` specialist. Date: 2026-10-15.
- Method: code reading, plus live probes on a private PostgreSQL database built from this worktree's migrations. The probes ran
  as the real `igaming_runtime` role, and as the table owner where noted. Go probes were added through `go test -overlay`, so no
  file in the repository changed. The full suites passed against the private database: `internal/alerting/...`,
  `internal/egressguard`, `internal/config`, `internal/observability`, `internal/reconciliation`, `cmd/platform-api`,
  `deploy/observability`, and `internal/httpserver -run 'FourEyes|AlertRout'`.
- I read the record's section 7 probe list, but I did not rely on it. Every claim below was checked again.

## Verdict

**CONDITIONAL.** I found one MEDIUM finding (SR9-A9-1). It blocks marking the A9 HTTP metric alerts as complete. Fix it, or
withdraw those alert rules and the runbook's "labels are bounded / no request data" claim until it is fixed. The other findings
are LOW or INFO. They are recommended hardening and do not block A9.

Nothing in this branch blocks launch by itself. The four-eyes rule (SR-7) is only as strong as the runtime role. That depends
on the pre-existing NULL-ARM-WRITE-1 item (see "Pre-existing context"), which is still a human decision.

## Findings

### SR9-A9-1 MEDIUM: an unauthenticated client can blow up metric labels through the Host header, which blinds the HTTP alerts

`otelhttp.NewHandler` (internal/httpserver/server.go) records `server.address` and `server.port` on the HTTP server metrics.
It takes both values from the request's `Host` header, which the client controls. A9 now sends these series to Prometheus,
and the A9 rules depend on them:

- the 5xx ratio rule uses `http_server_request_duration_seconds_count{http_response_status_code=~"5.."}`;
- the readiness rule uses `{http_route="/readyz",http_response_status_code="503"}`.

**Repro** (overlay test against `observability.InitMetricsExposed` plus `otelhttp`, 3000 requests with distinct `Host`
values):

- The exposition grew to **28.8 MB and 106,000 `http_server*` lines**.
- Series are labelled `server_address="victim-1.attacker.example",server_port="1001"`.
- Once the SDK's per-instrument cardinality limit (2000) is reached, new requests land in
  `{otel_metric_overflow="true"}`. That series has **no** `http_response_status_code` and no `http_route`.

**Failure scenario.** An attacker sends about 2000 requests to the public API, each with a different Host header. From then
until the process restarts:

- 5xx responses are counted in the ratio's denominator but not its numerator, so the ratio is under-reported;
- `/readyz` 503s are invisible to the readiness rule;
- every scrape carries tens of MB.

The runbook (`alert-delivery-channels.md` section 6) says the exposition "contains no request data; labels are bounded". That
is false today.

**Fix:**

- Add an `sdkmetric.NewView` for the `http.server.*` instruments with an `attribute.NewAllowKeysFilter`. Allow
  `http.request.method`, `http.response.status_code`, `http.route` and optionally `url.scheme`. Drop `server.address`,
  `server.port` and `network.*`.
- Add a test that sends N distinct Host headers and asserts that no `server_address` label appears and that the series count
  stays bounded.
- Correct the runbook.

### SR9-A9-2 LOW: the egress guard's IPv6 denylist misses some special ranges

`egressguard.PublicAddr` treats these as public (live run of the package):

- `::/96` IPv4-compatible, e.g. `[::127.0.0.1]` and `[::169.254.169.254]`;
- `::ffff:0:0/96` IPv4-translated;
- `2001:10::/28` and `2001:20::/28` (ORCHID);
- `2001:2::/48` (benchmarking);
- `3fff::/20` (documentation, RFC 9637);
- `5f00::/16` (SRv6 SIDs);
- `192.88.99.0/24` (old 6to4 relay).

`ValidateURL` also accepts these as URL literals. Network-specific NAT64 prefixes, which an operator chooses per network, are
not covered either: only `64:ff9b::/96` and `64:ff9b:1::/48` are.

Current Linux kernels do not route `::/96` to IPv4, and the sandbox has no IPv6, so I could not show that any of these
actually connects. That is why this is LOW.

**Fix:** for IPv6, accept only `2000::/3` and then subtract the special-purpose ranges, instead of a pure denylist. Add
`::/96` and `::ffff:0:0/96` explicitly. In the runbook, document that a deployment with a NAT64 network-specific prefix must
add that prefix.

Everything else I probed was refused, at the right layer:

- **Literals and encodings:** v4-mapped, NAT64 well-known, 6to4, Teredo, CGNAT (including 100.100.100.200), 169.254.169.254,
  `fd00:ec2::254`, `0.0.0.0`/`0`, decimal, octal and hex forms, `127.1`, percent-encoded hosts, IDN in U+3002 form, zone ids.
- **URL shape:** userinfo, `@`/backslash tricks, fragment, empty port, port 0, port 65536, `http` outside development.
- **Names:** `localhost`, `*.localhost`, `.internal`/`.local` names.
- **DNS:** a name that resolves to loopback (`vm` in /etc/hosts) is refused **at dial time**, with class `misconfigured`
  (live).

Redirects are not followed. `Proxy` is nil, so the transport ignores `HTTP(S)_PROXY`. The TLS minimum is 1.2 with default
verification. All timeouts are bounded.

### SR9-A9-3 LOW: SR-7 cannot tell an approval request from any other disabled route

The guard in 0156 approves against **any** current, disabled route with the same parameters that a different admin created in
the last 7 days. That includes a route the other admin disabled for `decommission`. The guard also does not compare
`effective_from`.

**Repro** (live): admin B supersedes a pager route with a disabled `decommission` version. Admin A, acting alone, then enables
the identical configuration with `staged_route_id` = B's row and `effective_from = now() - 365 days`. The insert is accepted.

The impact is limited: the recipient and URL come from platform configuration, and A only re-enables what B had. But "B
created a disabled row" is not the same as "B proposed this for enablement".

**Fix:**

- Mark staging intent explicitly: a `reason_code` in a staging set, or a `staged_for_approval` column.
- Force `effective_from := now()` on enabling versions, or compare it with the staged row.

Verified good (live):

- the same admin in two sessions is refused, with the "approver must differ" message;
- a reused staged id is refused by the `alert_routes_staged_route_once` unique index;
- mismatches in recipient, severity, step or channel are refused;
- a `staged_route_id` on non-human or disabled versions is refused;
- the guard reads `alert_routes` under the caller's RLS, and a session without a GUC sees 0 rows, so the guard fails closed;
- `tenant_id` is NULL by CHECK, so there is no cross-tenant route.

### SR9-A9-4 LOW: the database does not check the `reconciliation.sweep_failed` payload, and platform admins can raise the Kind

`alerts_guard` enforces the attribute shape and a forced discriminator only for `alerting.raise_failed`. For
`reconciliation.sweep_failed`, only the attribute **keys** are checked.

**Repro** (live, runtime role with a forged `app.platform_service_id=alert_dispatcher`): a P1 with
`{"phase":"player jane.doe@example.com card 4111111111111111","sqlstate_class":"anything at all"}` was accepted.

In addition, the `alerts_platform_admin` policy (FOR ALL, no kind restriction) lets any platform-admin session raise
`reconciliation.sweep_failed` (live: accepted). This contradicts the 0156 header, which says "platform-admin raise paths for
it do not exist".

Tenant sessions are refused correctly (subject/scope triggers and RLS; live). The dispatcher identity still cannot raise
other non-meta Kinds (live: `staff.platform_break_glass_used` was refused by RLS).

**Fix:**

- In `alerts_guard`, for this Kind: `phase` must be in the closed set, `sqlstate_class` must match `^[0-9A-Z]{2}$`, and the
  discriminator is forced to `'phase:' || phase`.
- Either correct the migration comment or restrict the platform-admin policy's INSERT kinds.

### SR9-A9-5 LOW: the table owner can delete eligible alerts without going through the purge function or its audit

`alerts_platform_admin` is FOR ALL, so it also grants DELETE. `alert_retention_delete_guard` checks only three things: the
flag, `current_user` = owner, and row eligibility.

**Repro** (live, owner role): with a platform-admin GUC and `SET app.alert_retention_purge=on`, a plain
`DELETE FROM alerts WHERE ...` removed an eligible resolved alert. No `alert_retention_runs` row was written. Ineligible rows
were still refused (AR201).

This matters only if the owner role is ever used outside migrations. The application uses a single `DATABASE_URL`, and the
deployment expects that to be the runtime role.

**Fix:** in the trigger, also require the dispatcher service GUC and the absence of a platform-admin GUC. Alternatively, have
the function set a transaction-local marker that a plain owner session would have to fake, or make the platform-admin policy
command-specific.

### SR9-A9-6 LOW: the "no PII" claim for the payload is overstated

The webhook body and the email carry the discriminator. Production discriminators include `staff:<staff uuid>`,
`break_glass:<staff uuid>`, `withdrawal:<id>`, `payout_instrument:<id>`, `attempt:<id>` and `receipt:<id>`. These are
pseudonymous identifiers. Under GDPR they count as personal data once they can be linked to a person, and here they go to a
third-party webhook receiver.

Exposing `subject_tenant_id` (an opaque tenant UUID) is acceptable.

No attribute values, error text, URL or secret are sent or logged (verified: only error classes are logged).

**Fix:**

- Document these as pseudonymous identifiers, not "no PII".
- Require a data processing agreement (DPA) with the webhook receiver's vendor.
- Optionally send a keyed hash of the discriminator on the webhook.

### SR9-A9-7 INFO: the signature does not cover two headers

`Idempotency-Key` and `X-Alert-Id` are outside the HMAC. The body's `dedup_key` and `alert_id` are signed. Tell receivers to
dedupe on the signed `dedup_key`.

The rest of the signing scheme is as documented:

- the HMAC is computed over `timestamp + "." + body`;
- `VerifyWebhookSignature` uses `hmac.Equal` and rejects timestamps outside the window in both directions;
- the secret is fetched by reference, the bytes are copied, and it is never logged or put in a header;
- secrets shorter than 32 bytes fail closed;
- the 5-minute cache means a rotation needs an overlap on the receiver side, which is documented.

The idempotency key `<alert_id>:<step>` stays the same across retries. Every header value is a UUID, a timestamp or passed
through `safeToken`, so CRLF injection was neutralised (live).

### SR9-A9-8 INFO: purge audit and interval semantics

`alert_retention_runs` records no caller identity: no `session_user`, `application_name` or backend information. A forged
invocation, which the record already lists as a residual, therefore cannot be told apart from a real one. Forging was
verified live: the runtime role with a forged dispatcher GUC ran the purge.

`p_older_than = '1 mon -23 days'` passes the 7-day check, because PostgreSQL normalises the month to 30 days. In February,
however, the computed cutoff is only about 5 days back. The per-row delete guard still enforces the 7-day floor, so the call
aborts and nothing is lost. The Go path passes seconds only.

**Fix:** refuse intervals that have a month component, and record `session_user` and `application_name` in the audit row.

Verified good (live):

- the purge is refused with no identity, with a tenant identity and with a platform-admin identity;
- a window under 7 days and batch values of 0 and 50001 are refused;
- only resolved alerts older than 7 days, with their occurrences, are deleted; open, acked and 6-day-old resolved alerts
  remain;
- the flag is reset to `off` afterwards;
- the runtime role has no DELETE or TRUNCATE on any alert table, and no INSERT, UPDATE, DELETE or TRUNCATE on
  `alert_retention_runs`;
- the function's `search_path` is pinned to `pg_catalog, public, pg_temp`, and the runtime role has no CREATE on `public`;
- EXECUTE is granted only to the owner and `igaming_runtime`;
- the downgrades refuse with AR099: 0157 refuses while audit rows exist, and 0156 refuses while email/webhook or staged routes
  exist.

### SR9-A9-9 INFO: configuration edge cases

- `APP_ENV=staging` accepts a non-loopback `METRICS_ADDR` without the acknowledgement. Production, and an unset `APP_ENV`,
  are strict.
- Single-label webhook hosts such as `metadata` pass the static URL check. The dial-time guard still applies.
- An empty fragment (`#`) is accepted. This is harmless because Go strips it from the request.

Verified good:

- the port is compared with `HTTP_ADDR`'s;
- the metrics server answers GET and HEAD on `/metrics` only (POST, OPTIONS and TRACE get 405; other paths, including
  `/debug/pprof/` and `//metrics`, get 404) and runs on its own `http.Server`;
- it uses a private registry and `MaxRequestsInFlight=4`;
- the A9 metric labels are closed sets with no tenant label;
- an email channel backed by MOCK is refused when the guard environment is production;
- recipients are validated as bare addresses;
- the subject and body go through `safeToken`;
- the webhook URL and secret are never logged.

## Pre-existing context (not introduced by A9)

- **NULL-ARM-WRITE-1** (registry, OPEN): verified again live. With **no GUC at all**, the runtime role inserted two
  `platform_admin` `staff_users` rows. It then staged an email/webhook route as admin A and enabled it as admin B. So SR-7
  stops a single admin on the honest application path, but it does not stop arbitrary SQL run as the runtime role or as the
  `alert_dispatcher` identity, which A9 uses more. This needs no new registry item, but the SR-7 documentation should say so.
- **Purge and sweep_failed under a forged dispatcher GUC:** the retention purge and the `sweep_failed` raise can be invoked
  with a forged dispatcher GUC. Like every alert RLS family, this identity is asserted by GUC. This is acknowledged in 0157.

## Dependencies (go.mod)

New modules:

- `prometheus/client_golang` v1.24.1;
- `otel/exporters/prometheus` v0.68.0;
- transitive: `client_model` v0.6.2, `common` v0.70.1, `otlptranslator` v1.0.0, `procfs` v0.21.1, `beorn7/perks`,
  `munnerz/goautoneg`, `google.golang.org/protobuf` v1.36.12, plus go.sum-only `klauspost/compress`, `kylelemons/godebug` and
  `go.yaml.in/yaml/v2`.

All come from the expected upstream publishers. `go mod verify` reports "all modules verified", and `GOSUMDB=sum.golang.org`
was active, so the hashes were checked against the checksum database when the modules were downloaded. No module name looks
like a typosquat.

The versions are past the CVEs I know of: client_golang CVE-2022-21698 (fixed in 1.11.1), protobuf CVE-2024-24786 (fixed in
1.33.0), and the x/net and compress issues predate these pins.

I could not run govulncheck because its database is unreachable from the sandbox. **Run it in CI before merge.**

## Defaults

Leaving retention off by default is safe from a security point of view: nothing is deleted, and startup warns. It is a
growth/availability concern and a GDPR storage-limitation question. Choosing the horizon is a legal decision for the human,
as the record says.

## Out of scope / not verified

- Real webhook receivers and a real email vendor (none exist).
- Live IPv6 dialing (the sandbox has no IPv6).
- govulncheck.
- Prometheus server and Alertmanager behaviour.
- The network controls of the deployed metrics port.
- Penetration testing.

Passing this review does not mean the subsystem is "secure". It means the items above were checked at code and design level
on this commit.
