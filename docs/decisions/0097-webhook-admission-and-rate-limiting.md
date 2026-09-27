# ADR 0097 — Webhook Admission and Rate Limiting (PAYWH-RL-1)

- **Status:** PROPOSED — design only, 2026-09-27. Not implemented. Nothing in this ADR
  is `IMPLEMENTED` until PRH-I4 lands and passes security review.
- **Decision type:** architecture + security control (cross-domain: `httpserver`,
  `webhookauth`, `identity`, `config`, the three webhook domains `payments`, `casino`, `kyc`).
- **Owner:** `security`. **Reviewers:** `devops` (configuration, deployment topology),
  `payments` (retry semantics, LF-C1), `architect` (cross-domain; dependency on ADR 0095's
  adapter manifest), `ledger-finance` (idempotency / no-side-effect guarantee), `qa` (§11).
- **Registry:** PRH-D3 (this ADR), PRH-I4 (implementation). Closes PAYWH-RL-1 when
  implemented and reviewed.
- **Related:** ADR 0022 §3 (callback contract, point 9 / I1), ADR 0091 (webhook trust
  hardening, uniform 401), ADR 0094 (two-phase `VerifyCallback`, INV-POOL; §4.3 explicitly
  deferred "general request-volume overload" to rate limiting — this ADR), ADR 0095
  (provider-I/O boundary, adapter capability manifest — dependency in §6.3), F-POOL-2,
  LF-C1.
- **Labels used below:** every default in §9 is a **technical default** chosen by
  engineering, not a legal, regulatory or contractual value. Nothing here is a Blueprint
  requirement unless stated; the control itself is required by CLAUDE.md "Security" and
  by the human's PRH instruction (PAYWH-RL-1 in scope).

## 1. Context — what exists today (verified at `1560ad0`)

Three unauthenticated provider-facing routes:

| Route | Handler | Body cap |
|---|---|---|
| `POST /v1/webhooks/payments/{tenantSlug}/{providerID}` | `deposit_handlers.go` `newPaymentWebhookHandler` | 1 MiB |
| `POST /v1/webhooks/casino/{tenantSlug}/{providerID}` | `casino_handlers.go` `newCasinoWebhookHandler` | 1 MiB |
| `POST /v1/webhooks/kyc/{tenantSlug}/{providerID}` (only when `KYCWebhookEnabled`) | `kyc_admin_handlers.go` `newKYCWebhookHandler` | 256 KiB |

Per-request pipeline today (`webhook_preamble.go`, then the handler):

1. path values present;
2. `webhookauth.CheckInboundPreamble`: provider-id charset → bounded body read
   (`io.LimitReader`, maxBody+1) → scheme lookup in the process-global adapter registry →
   `scheme.Extract` (headers only). **No DB.**
3. `identity.GetTenantBySlug` — **one pooled connection, platform-wide, unauthenticated.**
4. tenant `status == active`.
5. `VerifyCallback(ctx, deps.DB, …)` — one or two short `WithTenantReadOnly` transactions
   (payments: `ProviderAcceptsWebhook` + `HandleReadSQL`; casino/KYC: `HandleReadSQL`),
   then the secret-store Fetcher with **no** connection held (ADR 0094 INV-POOL).
6. `deps.DB.WithTenant` → `ReceiveVerifiedCallback` (re-check, parse, domain, ledger).

Gaps this ADR closes (security findings, severity against the dev-stage platform):

- **RL-F1 (Medium, pre-existing):** steps 3 and 5 run for any unauthenticated caller who
  names an active slug and a registered provider with well-formed headers. There is no rate
  or concurrency bound on this DB work. ADR 0094 made its *duration* independent of the
  secret store; its *volume* is unbounded. The existing per-IP limiter (`ratelimit.go`)
  covers only the auth endpoints.
- **RL-F2 (Medium, pre-existing):** a verified tenant (or a compromised tenant credential)
  can open unbounded concurrent domain transactions. A domain transaction waiting on a row
  lock (e.g. many callbacks for one wallet) holds a pooled connection while it waits
  (pool `DatabaseMaxConns` = 10). One tenant can therefore pin the pool for every tenant.
  That is the same cross-tenant impact class as F-POOL-1, from request volume.
- **RL-F3 (Medium, pre-existing, platform-wide):** `cmd/platform-api/main.go` sets only
  `ReadHeaderTimeout: 5s`. There is no `ReadTimeout` or `IdleTimeout`, so a slow body sender
  holds a goroutine (and, after this ADR, an admission slot) indefinitely. This ADR fixes it
  for webhook routes (§5 step A5). The platform-wide fix is registered separately as
  **HTTP-TIMEOUTS-1** (proposed; see §13).
- **RL-F4 (Low):** `loggingMiddleware` logs `r.URL.Path` verbatim. On webhook routes the
  path segments are attacker-chosen, and their length is bounded only by `MaxHeaderBytes`
  (1 MiB default). This amplifies log volume, and random-slug floods write attacker strings
  into every `http_request` line. Fix in §8.

## 2. Requirements (from the human, PRH-D3) → where each is met

| Requirement | Met by |
|---|---|
| tenant-aware, provider-aware, endpoint-aware | keys in §4 (domain × tenant × provider, at both tiers) |
| bounded memory and **key cardinality before verification** | §4.2: pre-auth keys come only from finite, server-owned sets; unknown values are collapsed |
| fail-safe | §7 |
| no tenant can starve another | §4, §6.4; residual R1 is stated honestly in §12 |
| does not bypass signature verification | §3: limiting only ever *rejects*; there is no allow-list, no "trusted source skips verify" path |
| no DB connection or resource-exhaustion vector; no DB work before admission | §3 ordering; A4 DB gate; test T4 |
| legitimate provider bursts | GCRA token bucket with burst (§5.1) |
| safe observability | §8 allow-listed fields, bounded labels, log suppression |
| preserves idempotency; a limited callback is retryable and has no side effects | §6: every rejection precedes the domain transaction; test T6 |

## 3. Decision — ordering (normative)

```
 network admission / rate control      A1..A5  (no DB, no body parse; body read only after A1-A3)
   -> authentication / verification    preamble + GetTenantBySlug + VerifyCallback, DB under A4 gate
   -> tenant/provider binding          VerifiedCallback (tenant_id, provider_id) = verified identity
        -> verified admission          B1 bucket, B2 per-tenant domain bulkhead
   -> parsing                          ReceiveVerifiedCallback: Recheck, HandleCallback (payload parse)
   -> domain processing                ledger / state machine / audit (same WithTenant tx)
```

Concrete order per request (each step runs only if the previous one admitted):

| # | Step | DB? | Body read? | Rejection |
|---|---|---|---|---|
| A0 | net/http: `ReadHeaderTimeout` (exists); route match | no | no | — |
| A1 | derive pre-auth key (§4.2): pure map lookups | no | no | — |
| A2 | per-source-IP bucket (**disabled by default**, §4.4) | no | no | 429 |
| A3 | pre-auth bucket `(domain, tenantKey, providerKey)` | no | no | 429 |
| A4a | webhook in-flight bulkhead acquire (global + per-tenantKey share), non-blocking | no | no | 503 |
| A5 | `Content-Length > maxBody` → reject without reading; body read under a per-request read deadline | no | yes (bounded) | uniform 401 (`body_too_large`, as today) |
| — | provider-id charset, scheme lookup, `Extract` (unchanged preamble) | no | — | uniform 401 |
| A4b | **pre-verification DB gate** (global + per-tenantKey), bounded wait, wraps `GetTenantBySlug` and every `TenantReader.WithTenantReadOnly` call made by `VerifyCallback` | gate first, then DB | — | 503 |
| V | `GetTenantBySlug`, active check, `VerifyCallback` (unchanged semantics) | yes, gated | — | uniform 401 |
| B1 | verified bucket `(domain, tenant_id, provider_id)` | no | — | 429 (or adapter-declared, §6.3) |
| B2 | per-tenant domain-transaction bulkhead, bounded wait, **no connection held while waiting** | no | — | 503 |
| D | `WithTenant` → `ReceiveVerifiedCallback` (Recheck → parse → domain) | yes | — | existing codes |

Invariants (each is pinned by a test in §11):

- **ORD-1.** No DB statement is issued for a request rejected at A1–A4a. Tests: statement
  capture (T10), plus a pool counter.
- **ORD-2.** No byte of the body is read for a request rejected at A1–A4a (T10).
- **ORD-3.** Every rate or capacity rejection happens strictly before `deps.DB.WithTenant`
  opens the domain transaction. No limiter runs inside or after the domain transaction.
  A 429 or 503 therefore never follows a commit (T6).
- **ORD-4.** Limiting never admits more than verification would. There is no bypass,
  allow-list or "known provider IP skips verification" path. B1/B2 run only on a
  `VerifiedCallback` produced by `VerifyCallback`.
- **ORD-5.** The number of concurrently held pooled connections attributable to
  pre-verification webhook work is ≤ `W_db` (A4b), whatever the request volume or
  store latency (T4).

"Binding" note. `GetTenantBySlug` runs before verification only to *select the credential*.
The slug→tenant result is untrusted until the signature (which covers the tenant, ADR 0022
§3) verifies. The trusted tenant/provider binding is the `VerifiedCallback`, and only B1/B2
key on it. The slug lookup is gated by A4b like any other pre-verification DB work.

## 4. Keying and cardinality

### 4.1 Why URL values cannot be keys as-is

`{tenantSlug}` and `{providerID}` are attacker-chosen. Keying a limiter on them directly
gives an attacker unbounded map growth, and a per-value fresh budget (random slugs = infinite
budget). Collapsing everything into one shared bucket per endpoint lets a flood aimed at
tenant A (or at non-existent tenants) exhaust tenant B's admission. Both are rejected.

**Keyed-hash shards rejected.** Keying by `HMAC(k, slug) mod K` bounds cardinality, but a
random-slug flood spreads over *all* shards and starves every tenant. It fails the
cardinality-attack test by construction.

### 4.2 Pre-auth key (A3) — bounded by server-owned sets

`preKey = (domain, tenantKey, providerKey)` where

- `domain` ∈ {`payments`, `casino`, `kyc`} — fixed by the route, never from input.
- `providerKey` = `providerID` **iff** `ValidProviderID(providerID)` and the domain
  orchestrator's `WebhookScheme(providerID)` is registered. That registry is process-global
  and built at startup, so the set is finite. Otherwise `providerKey = "_unknown"`.
- `tenantKey` = `tenantSlug` **iff** `len(tenantSlug) ≤ 128` and the slug is present in the
  **webhook tenant directory**. Otherwise `tenantKey = "_unknown"`.
- If either component is `_unknown`, the whole key collapses to `(domain, "_unknown",
  "_unknown")`. There is **one** unknown bucket per domain.

**Webhook tenant directory** (new, `httpserver`): an in-memory, copy-on-write snapshot
(`atomic.Pointer[map[string]struct{}]`) of active tenant slugs.

- Loaded once, synchronously, at startup. It is refreshed every `DirectoryRefresh` (30 s) by
  one background goroutine, using one `WithoutTenant` query:
  `SELECT slug FROM tenants WHERE status = 'active' ORDER BY slug LIMIT $cap`.
  `tenants_read` is `USING (true)`, as `GetTenantBySlug` already relies on.
- **Not request-driven:** no request can trigger a refresh, so it is not an attacker-reachable
  DB vector.
- **Non-authoritative:** the directory is used *only* to choose a limiter key. It never
  authorizes anything, and `GetTenantBySlug` + the active check + verification remain the
  authority. A suspended tenant still in a stale snapshot gets its own bucket and then the
  uniform 401. A tenant created since the last refresh is keyed `_unknown` for ≤ 30 s. Its
  callbacks are still processed if the unknown bucket admits them, and are otherwise
  retryable 429s (§6).
- `cap` = 10 000. Tenants beyond the cap are keyed `_unknown`, with one `error` log line per
  refresh (`webhook_tenant_directory_truncated`). This is misconfiguration territory, not a
  normal state.

**Cardinality bound (pre-auth):** `|keys| ≤ Σ_domain (T_dir × P_domain + 1)`. `T_dir` ≤ 10 000
directory slugs; `P_domain` = registered adapters in that domain. The bound does not depend
on request input. Entries are created lazily and evicted after `IdleEvict` (10 min) at full
tokens. With GCRA (§5.1) each entry is one `int64`, plus the key.

### 4.3 Verified key (B1, B2)

- B1: `(domain, tenant_id, provider_id)` taken from the `VerifiedCallback`.
- B2: `tenant_id`.

These keys exist only after a successful signature verification, so an unauthenticated
caller cannot create them. Hard cap `VerifiedMaxKeys` = 30 000. Beyond it, new keys share one
`_overflow` bucket with the unknown-bucket parameters and one `error` log. The table is
**never reset**: the existing auth limiter's reset-on-full fail-open is deliberately **not**
copied (§7).

### 4.4 Per-source-IP tier (A2) — present, disabled by default

It uses `trustedProxyClientIP(r, TrustedProxyCount)`, the same trust model as `ratelimit.go`.
It is **off by default** (`WEBHOOK_RL_PER_IP_RPS=0`) for two reasons:

1. **Behind a load balancer with `TRUSTED_PROXY_COUNT=0`**, every request has the LB's
   address. A per-IP bucket then becomes one *global* bucket, which is exactly the
   cross-tenant starvation lever this ADR removes.
2. **Providers send callbacks for all their tenants from a small shared egress-IP set.** A
   per-IP budget must therefore cover a provider's aggregate across every tenant, and at that
   size it barely limits an attacker.

It is enabled only by deployment configuration once the edge topology is known (WEBHOOK-EDGE-1,
§13). When enabled it runs first, so an IP-limited request consumes no tenant tokens. Its
map is bounded (`PerIPMaxKeys` = 50 000). Beyond that, new IPs skip **only this tier**, with a
logged `error`. A3/A4/B1/B2 still bound everything, so this one tier failing open never
removes the DB or tenant bounds.

## 5. Mechanisms

### 5.1 Token bucket: GCRA with burst

Each key stores one theoretical arrival time (TAT, `int64` ns). The emission interval is
`T = 1s / rate` and the burst tolerance is `τ = T × (burst − 1)`. A request is admitted iff
`now ≥ TAT − τ`; then `TAT = max(TAT, now) + T`. On rejection,
`Retry-After = ceil((TAT − τ − now) / 1s)`, clamped to [1, 60].

GCRA is exactly a token bucket (rate, burst). It uses integer arithmetic, so there is no
float drift. It is O(1), and it is deterministic under an injected clock: every bucket,
bulkhead wait and suppression window takes a `clock` interface (`Now()`,
`NewTimer(d)`), and tests use a fake clock.

### 5.2 Keyed bulkhead (A4a, A4b, B2)

This is one primitive: a counting semaphore with a global cap `G`, a per-key cap `K ≤ G`,
and a separate cap `U` for the `_unknown` key.

- A4a is non-blocking.
- A4b waits at most `DBGateWait` (100 ms).
- B2 waits at most `DomainWait` (2 s).

Waiting is goroutine time only. A4b is acquired **before** the pool acquire and released
**after** the transaction commits or rolls back. B2 is acquired **before**
`deps.DB.WithTenant`. Per-key counters exist only while > 0, so their cardinality is ≤ `G`.
Release is idempotent (`sync.Once` per acquisition). A double release is a test failure,
never a negative count.

### 5.3 A4b wiring (no new pre-verification statement)

- `webhookPreamble` wraps its `GetTenantBySlug` call in `gate.Acquire(tenantKey)`.
- The handlers pass `gatedReader{db: deps.DB, gate, tenantKey}` (it implements
  `webhookauth.TenantReader`) to `VerifyCallback` instead of `deps.DB`.

The ADR 0022 §3 point-9 statement set is unchanged. The `TestPointNineCapture_*` tests stay
the primary I1 control, and the directory refresh is not a per-request statement. INV-POOL
still holds: the gate wraps only the read-only transactions, never the secret-store fetch.

### 5.4 Body (A5)

- If `Content-Length` is present and exceeds `maxBody`, the request is rejected before any
  read (same uniform 401 / `body_too_large` as today: no new oracle).
- Chunked bodies keep today's `LimitReader(maxBody+1)`.
- `http.NewResponseController(w).SetReadDeadline(now + BodyReadTimeout)` is set at
  admission, with `BodyReadTimeout` = 10 s. A slow sender loses its A4a slot at the deadline.
- Memory bound: `G_inflight × maxBody` = 64 × 1 MiB = 64 MiB worst case for webhook bodies.

## 6. Status codes, retries and idempotency

### 6.1 Codes

| Rejection | Status | Retry-After | Body |
|---|---|---|---|
| A2/A3 bucket empty | **429** `rate_limited` | computed (§5.1) | generic `"too many requests; retry later"`, request id only |
| A4a / A4b capacity | **503** `service_unavailable` | 1 s | generic |
| B1 verified bucket | **429** (default) or adapter-declared (§6.3) | computed | generic |
| B2 domain bulkhead | **503** | 2 s | generic |
| limiter panic or internal error | **503** (the admission layer recovers itself; it is never a 500 from `recoverMiddleware`) | 1 s | generic |

**Never** 400, 401, 404 or 409 for limiting. Those collide with the uniform-401 contract and
with terminal client-error semantics. **Never** 200/2xx: acknowledging an unprocessed event
is silent event loss.

### 6.2 Relation to the uniform-401 contract (ADR 0091, T9)

A3/A4 responses are a new **pre-verification** response class. Their outcome depends only on
limiter state for `preKey`, never on signature validity, key id, or anything verification
computes. With the same bucket state, a validly signed and an invalidly signed request get
byte-identical responses, request id aside (test T15). They are therefore not a signature
oracle. The one information leak (whether a slug is in the active directory) is residual R2.

B1/B2 responses reach only verified callers.

### 6.3 Provider retry semantics (LF-C1)

A limited callback is only safe if the provider redelivers it. The platform cannot assume
this: vendors differ, and some may treat a 4xx as terminal for win, rollback or reversal.

- **MOCK adapters:** 429 / 503 as in §6.1.
- **Every non-MOCK adapter must declare, before registration,**
  `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter, RetryWindow}`. This
  belongs in the adapter capability manifest that ADR 0095 designs (dependency; `architect`
  to place it). Until ADR 0095 lands, it is a per-provider config key.
  - If the adapter does not retry 429, B1 answers with **503** for that provider.
  - If it retries neither 429 nor 503, the adapter must not be registered for webhook
    delivery without LF-C1 option (b): daily reconciliation that detects provider-settled,
    platform-unposted events with a P1 alert.
  - A non-MOCK adapter with no declaration fails registration (fail closed).
- **`RetryWindow`:** sustained limiting longer than a vendor's retry window loses the event
  from the push channel. Reconciliation (LF-C1 (b), PRH-I5 for payments) is the backstop,
  and the `webhook_admission_rejected` metric per verified key feeds an alert (§8).

This ADR does not assert any real vendor's behaviour. Each value is recorded at that
vendor's integration gate.

### 6.4 Idempotency and ordering effects

- **No side effects.** By ORD-3, a limited request writes nothing: no ledger entry,
  tombstone, audit row, casino rejection record, KYC state change or intent transition. The
  redelivery is processed as a first delivery. If an earlier delivery *did* commit (a
  provider retrying for its own reasons), the existing `(provider_id, provider_tx_id)`
  unique constraint and payload-mismatch checks apply unchanged.
- **Signature timestamp:** a redelivery of the same signed bytes after `Retry-After` (≤ 60 s)
  stays well inside the ≤ 10 min skew window (ADR 0094 §5). Most vendors re-sign anyway.
- **Reordering:** limiting can delay an original past its rollback (casino: bet 429'd,
  rollback admitted → tombstone → the retried bet is rejected). That is the existing,
  ledger-correct tombstone semantics, not a new failure. It is listed so `casino` and
  `ledger-finance` confirm it (test T6c).

## 7. Fail-safe behaviour

| Condition | Behaviour | Rationale |
|---|---|---|
| Directory never loaded (startup) | startup waits for the first load; `/readyz` not-ready until then; webhook routes return 503 if called anyway | no traffic runs with unknown keying |
| Directory refresh fails | keep last snapshot; `error` log; metric `webhook_tenant_directory_age_seconds`; alert > 10 min | known tenants keep their buckets; never fall back to raw slug keys |
| Pre-auth key table cap (§4.2) | cannot be exceeded by input; if the configured bound is misestimated → `_overflow` shared bucket + `error` | degrade to bounded, never unbounded, never reset |
| Verified table cap | `_overflow` bucket + `error` | same |
| Per-IP table cap | only A2 skips; others still apply | §4.4 |
| Limiter panic | recovered inside admission → 503 | fail closed, retryable |
| Invalid configuration | **startup fails** (§9.3 validation) | no silently-unlimited deployment |
| Disabling | `WEBHOOK_ADMISSION_ENABLED=false` accepted **only** when test-support routes are enabled (non-production), mirroring existing gates; rejected in production config | an incident is handled by raising overrides, not by removing the control |

Deliberate contrast with `ratelimit.go` (auth). Its reset-on-full fails **open** by design,
and that is correct for auth: keys are real TCP source addresses, and an outage of login is
worse. Here, pre-auth keys are bounded by construction, so "full" means a bug. Failing open
would remove the DB bound exactly when it is under attack.

## 8. Observability (safe by construction)

**Logs.** One allow-listed `warn` event, `webhook_admission_rejected`, with exactly these fields:
- `request_id`, `domain`, `tier` (`ip|preauth|inflight|db_gate|verified|domain_bulkhead`);
- `tenant_key` (a directory slug or `_unknown`, **never** the raw path value when unknown);
- `provider_key` (a registered id or `_unknown`);
- `tenant_id` (tiers B1/B2 only);
- `status`, `retry_after_s`, `client_ip`, `suppressed` (count).

**Never logged:** headers, signature values, key material, raw body, body excerpts, unknown
path values, query strings.

**Suppression.** At most one line per `(tier, key)` per 10 s. The next emitted line carries
`suppressed=N`. The suppression state lives in the same bounded key space, so under a flood
log volume is ≤ keys / 10 s.

**`http_request` access line (RL-F4).** On webhook routes it logs `r.Pattern`
(`POST /v1/webhooks/payments/{tenantSlug}/{providerID}`) instead of `r.URL.Path`. The
existing auth-failure line already logs only the charset-validated provider id.

**Metrics** (OTel meter; no-op if unset). Label sets are bounded:
- `webhook_admission_decisions_total{domain, tier, outcome}`;
- `webhook_admission_verified_rejected_total{domain, provider_key}`. Per-tenant detail goes
  only to logs, because a `tenant_id` label would grow with tenant count;
- gauges `webhook_inflight{domain}`, `webhook_db_gate_in_use`, `webhook_limiter_keys{tier}`,
  `webhook_tenant_directory_size`, `webhook_tenant_directory_age_seconds`.

**Alerts** (documented for devops, not built here): sustained verified-tier rejections for a
key (> 5 min), which risks the vendor retry window; directory age > 10 min.

## 9. Configuration

### 9.1 Defaults (technical, reversible; to be re-measured in PRH-I4)

Basis: pool `N = DatabaseMaxConns = 10`; pre-verification DB work ≈ 2–3 short read-only
transactions of about 1–3 ms each (ADR 0094 §6); a single-tenant dev-stage deployment. The
per-key rates are "no single (tenant, provider) may take more than a modest fraction of
estimated pool throughput". They are **not** vendor volumes. PRH-I4 records a non-gating
benchmark, and the defaults are revisited before any real-provider gate.

| Parameter | payments | casino | kyc | Rationale |
|---|---|---|---|---|
| A3 pre-auth rate / burst per known key | 50/s / 200 | 300/s / 1000 | 10/s / 50 | ≥ 2× B1 so legitimate traffic is bounded by the verified tier (which attackers cannot drain), and A3 is a DB-cost backstop; casino carries synchronous bet traffic |
| A3 `_unknown` per domain | 2/s / 10 | 2/s / 10 | 2/s / 10 | absorbs directory lag for new tenants; bounds random-slug DB lookups to ≤ 2/s/domain |
| B1 verified rate / burst per (tenant, provider) | 25/s / 100 | 200/s / 800 | 5/s / 50 | burst covers a provider replaying a backlog after its own outage |

| Parameter | value | Rationale |
|---|---|---|
| A4a in-flight global / per tenantKey / `_unknown` | 64 / 16 / 4 | memory ≤ 64 × maxBody; one tenant ≤ 25 % |
| A4b DB gate `W_db` / per tenantKey / `_unknown` / wait | `max(1, ⌊0.3N⌋)` = 3 / `min(2, W_db)` / 1 / 100 ms | unauthenticated work holds ≤ 30 % of the pool; one tenant ≤ 2 connections |
| B2 per-tenant domain transactions / wait | `max(1, ⌊0.3N⌋)` = 3 / 2 s | one tenant's verified traffic holds ≤ 3 of 10 connections (also bounds F-POOL-2 pinning per tenant until ADR 0095 lands) |
| A2 per-IP | 0 (off) | §4.4 |
| `BodyReadTimeout` | 10 s | above any sane 1 MiB upload; bounds slowloris |
| `DirectoryRefresh` / cap | 30 s / 10 000 | new-tenant lag ≤ 30 s |
| `IdleEvict` / `VerifiedMaxKeys` / `PerIPMaxKeys` | 10 min / 30 000 / 50 000 | memory bound |

### 9.2 Overrides

Overrides are platform-operator configuration (env `WEBHOOK_ADMISSION_OVERRIDES`, JSON),
keyed by `domain` + `provider_id` and optionally `tenant` (slug for A3, id for B1). They set
rate and burst only.

They are **not** tenant/brand configuration rows and **not** partner-console editable. A
tenant raising its own limit would defeat cross-tenant fairness. This is a platform
operational control, not brand configuration, so the CLAUDE.md "brand differences are config
rows" rule does not apply. A staff-editable, audited store is deferred (WEBHOOK-RL-ADMIN-1)
until an operator actually needs runtime changes.

### 9.3 Validation (startup fails on violation)

- rate > 0 and burst ≥ 1;
- A3 rate ≥ B1 rate for the same (domain, provider) (otherwise legitimate traffic competes
  with attackers at A3);
- `0 < K ≤ G` for every bulkhead;
- `W_db < N`;
- B2 cap < N;
- overrides reference only known domains and charset-valid ids;
- no disabling in production.

## 10. Multi-instance

- A4a, A4b and B2 protect **per-process** resources (goroutines, memory, that process's pool),
  so per-process state is *correct*, not a compromise.
- A2, A3 and B1 per process give an effective platform limit of (limit × replicas), the same
  disclosure as `ratelimit.go`. That is acceptable now: there is one replica, and the goal is
  resource protection, not a contractual quota.
- CLAUDE.md's "Redis never holds an authoritative balance" rule concerns balances. It does
  not forbid a shared limiter.
- A shared limiter is **deferred, not built** (WEBHOOK-RL-SHARED-1). The trigger is either a
  need for a platform-wide per-tenant quota or uneven LB distribution observed across
  replicas. If built, it must degrade to the local limiter when the shared store is
  unavailable, and it must never sit on the balance path.

## 11. Adversarial test plan (QA to confirm)

All tests are **main lane**:
- unit tests: fake clock, no DB, `-race`;
- integration tests: `-tags=integration`, real DB;
- no wall-clock latency assertions.

Capacity properties are asserted by **counts under barriers** (blocking hooks inside the gated
section), not by elapsed time. Nothing joins the isolated timing lane: per the CI-342
ruling B, a new test there needs its own security ruling, and none is needed.

| # | Test | Asserts |
|---|---|---|
| T1 | `CrossTenantStarvation_PreAuth` | flood tenant A's valid slug with garbage signatures at 10× A3 burst; every one of B's correctly signed callbacks in the same interval → 200 and processed; A sees only 401/429/503 |
| T2 | `CrossTenantStarvation_Verified` | A sends *validly signed* callbacks beyond B1 and B2; A gets 429/503; B's callbacks are all admitted; A's rejected callbacks wrote zero rows (ledger, tombstone, audit, rejection record) |
| T3 | `CardinalityAttack_RandomPathValues` | 100 000 random slug/provider values (random length ≤ 4 KiB, unicode, charset-invalid): limiter key count ≤ §4.2 bound (exact); all logged as `_unknown`; `GetTenantBySlug` invocations ≤ unknown-bucket allowance under the fake clock; B's callbacks admitted |
| T4 | `UnauthFlood_DoesNotConsumePool` | real pool N = 10; a hook blocks inside the gated section; 200 concurrent unauthenticated requests with a valid slug; while blocked, `pool.Stat().AcquiredConns` attributable to webhooks ≤ `W_db` (gate counter plus pool stat), and an unrelated `pool.Acquire` succeeds without waiting on the barrier; requests beyond the gate get 503 and executed **zero** statements |
| T5 | `BurstAccommodation` | fake clock frozen: exactly `burst` admitted; `burst+1` → 429 with `Retry-After = ceil(1/rate)`; advance by one emission interval → exactly one more admitted; sustained `rate` never rejected |
| T6a/b/c | `RetryAfter429_Idempotent` (payments deposit; casino win; casino bet-then-rollback reorder) | limited attempt → zero rows; advance the fake clock past Retry-After; redeliver → 200 with exactly one posting; redeliver again → idempotent duplicate, no second posting; `SUM(debits)==SUM(credits)`, projection == rebuild; (c) tombstone semantics as §6.4 |
| T7 | `NoSecretOrRawInputInLogs` | canary secret in body, signature headers, random slugs; the captured logs contain none of them; admission log lines ≤ keys × (duration / 10 s) + 1 |
| T8 | `Races` (`-race`) | concurrent acquire/release per key; in-flight max (atomic high-water mark) ≤ cap; GCRA admitted count ≤ burst + rate × fake elapsed; directory swap concurrent with lookups; eviction concurrent with admission; release exactly once |
| T9 | `FailSafe` | refresh error → old snapshot kept; nil directory → 503 and `/readyz` not-ready; injected limiter panic → 503, not 500; table cap reached → `_overflow` used, no reset |
| T10 | `Ordering_NoDBNoBodyBeforeAdmission` | extends `TestPointNineCapture_*`: a request rejected at A2/A3/A4a issues zero statements and reads zero body bytes (instrumented reader); the point-9 statement set is unchanged for admitted requests |
| T11 | `SlowBody_ReleasesSlot` | a stalled body sender loses its A4a slot at the read deadline (short test deadline; asserts release with a generous upper bound, no latency claim) |
| T12 | `ConfigValidation` | each §9.3 violation fails startup; production cannot disable |
| T13 | `DomainIndependence` | a casino flood does not consume payments/KYC buckets or in-flight shares beyond the A4a global cap |
| T14 | `AdapterDeclaredStatus` | provider declared "no 429 retry" → B1 answers 503; undeclared non-MOCK adapter → registration fails |
| T15 | `PreAuthResponse_Indistinguishable` | same bucket state: valid- and invalid-signature requests → identical status, headers and body except request id |
| T16 | Mutations (recorded like ADR 0094 §9.4) | drop tenant from `preKey` → T1 fails; move B1/B2 inside `WithTenant` → T6 fails; remove collapse → T3 fails; remove A4b → T4 fails; log raw path → T7 fails |

Existing suites: high-concurrency integration tests (e.g. `stage9_concurrency_integration_test.go`)
must pass unchanged at the defaults. B2 queues rather than rejects within `DomainWait`. Any
test that needs larger limits sets them explicitly through `Deps` and records why.

## 12. Residual risks (disclosed, not hidden)

- **R1 — targeted pre-auth denial of one tenant (Medium, needs an edge decision before
  real-money launch).**
  - An unauthenticated attacker who floods tenant B's *own* valid URL drains B's A3 bucket
    and per-tenant A4 shares. Before verification, the attacker is indistinguishable from B's
    provider except by source address.
  - Other tenants are unaffected: that is the fairness guarantee.
  - B's callbacks are delayed, not lost, while the vendor retries, and reconciliation backs
    this up.
  - Real mitigation is at the edge: provider source-IP allow-lists, a WAF, or mTLS where the
    vendor supports it. That is WEBHOOK-EDGE-1 (devops + human; AWS is out of PRH scope).
- **R2 — directory-membership oracle (Low).**
  - With the per-domain `_unknown` bucket drained, a probe that is *not* 429'd reveals that
    a slug is an active tenant.
  - Accepted if tenant slugs are not confidential (they are configured at vendors and likely
    visible in brand URLs). **Question for the human:** are slugs confidential?
  - If yes, the mitigation is opaque per-tenant webhook path tokens, deferred as
    WEBHOOK-PATH-TOKEN-1.
- **R3 — simultaneous floods on many tenants.** k attacked tenants hold up to
  k × per-key share of A4a/A4b until the global caps. Healthy tenants then see 503s for the
  duration. Bounded, retryable, zero side effects; the same class as the ADR 0094 onset
  residual.
- **R4 — per-process limits × replicas** (§10).
- **R5 — player-facing and admin routes** are outside this ADR. B2 bounds only the webhook
  domain transactions, and RL-F2's pool-pinning class still exists for authenticated API
  routes. It is recorded as a scope note for the architect, not claimed as covered.

## 13. Implementation breakdown (PRH-I4; security + backend; devops for config)

| File | Change |
|---|---|
| `internal/admission/` (new leaf package; imports stdlib only) | `clock.go` (interface + fake), `gcra.go` (keyed GCRA, bounded map, idle eviction, overflow key), `bulkhead.go` (global/per-key/unknown caps, non-blocking + bounded wait, idempotent release), `suppress.go` (log suppression); unit + race tests |
| `internal/httpserver/webhook_admission.go` (new) | config struct and defaults, `preKey` derivation, A2/A3/A4a middleware, `gatedReader` (A4b), `admitVerified` (B1/B2), response writer (§6.1), allow-listed logging, metrics |
| `internal/httpserver/webhook_tenant_directory.go` (new) | snapshot, synchronous initial load, refresher lifecycle |
| `internal/identity/tenant.go` | `ListActiveTenantSlugs(ctx, pool, limit)` |
| `internal/httpserver/webhook_preamble.go` | A5 `Content-Length` check and read deadline; `GetTenantBySlug` under A4b; accept tenantKey |
| `deposit_handlers.go`, `casino_handlers.go`, `kyc_admin_handlers.go` | pass `gatedReader` to `VerifyCallback`; call `admitVerified` before `WithTenant`; hold B2 across any follow-up denial-audit transaction |
| `financial_routes.go`, `casino_routes.go`, `kyc_routes.go` | wrap the three webhook routes with the admission middleware (domain constant) |
| `server.go`, `health.go` | `Deps` fields; `/readyz` includes directory loaded |
| `middleware.go` | `r.Pattern` for webhook routes (RL-F4) |
| `internal/config/config.go`, `cmd/platform-api/main.go` | env vars, overrides JSON, §9.3 validation, refresher goroutine with shutdown |
| payments/casino/kyc adapter registration | `WebhookRetrySemantics` declaration (placement per ADR 0095; config key until then) |
| `docs/api/openapi/platform-api.yaml` + `openapi_*webhook_contract_test.go` | 429/503 + `Retry-After` on the three webhook paths |
| `docs/security/` | threat-model entry for webhook admission (security, at implementation review) |

**Lane:** main lane only (§11). Estimated as one change set, with no migration.

**Registry follow-ups** (orchestrator to register; not edited by this ADR):
- HTTP-TIMEOUTS-1 (RL-F3, platform-wide `ReadTimeout`/`IdleTimeout`);
- WEBHOOK-EDGE-1 (R1, before real-money launch);
- WEBHOOK-PATH-TOKEN-1 (R2, conditional on the human's answer);
- WEBHOOK-RL-SHARED-1 (§10, deferred);
- WEBHOOK-RL-ADMIN-1 (§9.2, deferred).

## 14. Consequences

- Unauthenticated webhook traffic has a rate bound (A3) and a connection bound (A4b)
  independent of volume. This closes the gap ADR 0094 §4.3 deferred.
- A verified tenant cannot take more than ~30 % of the pool through webhooks (B2).
- New response class: 429/503 with `Retry-After` on webhook routes. Vendors must retry them;
  that is enforced at adapter registration (§6.3).
- One background DB query every 30 s (directory). One new leaf package.
- **Launch-relevant:** R1 needs an edge-control decision (WEBHOOK-EDGE-1) before real-money
  launch. RL-F3 should be fixed platform-wide before any public exposure.

## 15. Review status

Security authors this design. Before it moves to ACCEPTED it needs concurrence from `devops`,
`payments` (§6.3), `architect` (the ADR 0095 manifest dependency; R5 scope), `ledger-finance`
(§6.4 / T6), and QA confirmation of §11. This document does not declare the control secure:
the security review of the PRH-I4 diff is still required.

## 16. QA test-plan review

**Reviewer:** `qa`. **Scope:** §11 test plan (T1–T16) only — not implementation, not
security correctness of the mechanisms themselves.

**Mapping check.** Every human-stated requirement has a named test with a measurable
(count/status-code/row-count, not elapsed-time) pass criterion: cross-tenant starvation
(T1 pre-auth, T2 verified, T13 domain independence), bounded (T3 cardinality, T8
high-water marks, table-cap tests folded into T3/T9), fail-safe (T9), no DB/pool
exhaustion (T4), legitimate bursts (T5), safe observability (T7), idempotency (T6a/b/c),
ordering (T10 for ORD-1/ORD-2, T6 for ORD-3, T4 for ORD-5 — see item 2 below for the
ORD-4 gap). The general PRH list is covered — unit (T5, T8, T9, T12, T15), integration/
PostgreSQL-backed (T4, T6, T10), race (T8, `-race`), concurrency (T1, T2, T4, T8, T13),
negative/security (T1, T2, T3, T7, T15), tenant isolation (T1, T2, T13), idempotency
(T6), failure injection (T9), timeout (T11, T4's gate-wait bound), retry (T6, T14),
duplicate callback (T6), connection-pool (T4) — with the gaps below.

**Verdict: CONFIRMED WITH CHANGES**

1. **T11 embeds a real wall-clock bound, contradicting §11's blanket "no wall-clock
   latency assertions" claim for the main lane.** `BodyReadTimeout` is enforced via
   `http.NewResponseController(w).SetReadDeadline`, which cannot be driven by the
   injected `clock` interface (§5.1) — unlike GCRA, bulkheads and suppression. T11
   therefore necessarily sleeps/waits on real time ("short test deadline... generous
   upper bound"). Per the human's requirement, any test with a real timing bound needs
   an explicit security ruling to run in a timing lane; it cannot silently stay in the
   unqualified main lane alongside the deterministic tests. Resolve one of two ways
   before PRH-I4: (a) parameterize `BodyReadTimeout` behind the same clock/timer
   abstraction if `net/http`'s deadline can be virtualized in tests (e.g. inject a
   fake `ResponseController`-like seam), keeping T11 fully deterministic and in the
   main lane; or (b) if that's not feasible, keep T11's real-time bound small and
   fixed (e.g. ≤ 200 ms), state that bound explicitly in §11, and route it through
   the CI timing lane with the security ruling the human's instruction requires — do
   not leave it merged into the "no wall-clock" main-lane set as currently written.

2. **No named test for ORD-4** ("limiting never admits more than verification would";
   B1/B2 key only off a `VerifiedCallback`). T1/T2 test the *outcome* of starvation but
   not the *invariant* that B1/B2 cannot be reached or keyed except through a
   successful `VerifyCallback`. Add `T17 NoPreVerificationBypass`: assert there is no
   code path (known IP, known provider, replayed pre-auth key, etc.) that reaches B1,
   B2 or the domain transaction without a `VerifyCallback` success in the same request,
   and that the B1/B2 key is read only from the `VerifiedCallback` value, never from
   `preKey`/URL values. Add a matching T16 mutation ("derive B1 key from `preKey`
   instead of `VerifiedCallback` → T17 fails").

3. **OpenAPI contract test is described in §13 but not present in the T1–T16 table.**
   §13 lists `openapi_*webhook_contract_test.go` for 429/503 + `Retry-After` on the
   three webhook paths, but nothing in §11 asserts it will exist or pass. Add
   `T18 OpenAPIContract_WebhookRateLimit`: the spec documents 429 and 503 (with
   `Retry-After`) on all three webhook paths, and representative admission responses
   from T1/T2/T9 validate against that schema (status, headers, and the generic §6.1
   body shape — no domain-specific fields leaking into the documented error body).

4. **No dedicated multi-tenant simultaneous-flood / connection-pool test for R3.**
   R3 ("k attacked tenants hold up to k × per-key share... until the global caps")
   is disclosed as a residual risk but never exercised. T4 tests one flood against the
   global pool bound, T13 tests domain isolation; neither drives several tenants past
   their per-key shares concurrently to confirm the *global* A4a/A4b/B2 caps (not just
   per-tenant caps) hold and that tenants outside the attacked set are unaffected. Add
   `T19 MultiTenantSimultaneousFlood`: k tenants (k × per-tenant share > global cap)
   flood concurrently; assert in-use A4a/A4b/B2 slots never exceed the documented
   global caps, excess requests get 503 with zero statements/rows, and a control
   tenant outside the k is admitted throughout. This turns R3 from an asserted-only
   risk into a measured one, consistent with the human's "bounded" requirement.

5. **"Repeated runs for concurrency" is not stated for the concurrency-sensitive
   tests.** §11 says "unit tests: fake clock, no DB, `-race`" but does not require
   T1, T2, T4, T8, T13 (and the new T17/T19) to run with a repeat count to catch
   flaky interleavings, as the PRH testing list requires. Add to §11: these tests run
   under `-race -count=N` (N ≥ 10, matching the project's existing stress-run
   convention) in CI, not just once.

6. **No stated CI time-budget check against the `internal/httpserver` main-lane
   budget (~310 s under the 10-min timeout).** T3 (100,000 synthetic values) and T4
   (200 concurrent requests) should stay sub-second to low-single-digit seconds since
   both use fake clocks and blocking-hook barriers rather than sleeps, but this is not
   verified anywhere in the ADR. Require PRH-I4 to record measured wall time for the
   full new webhook-admission suite (T1–T19) in `internal/httpserver` and flag to the
   orchestrator if the main lane's total approaches the 310 s figure. Recommend T1–T10,
   T12–T19 stay in the existing main-lane file(s); T11 is placed per item 1's outcome.

7. **Determinism claim needs a verification hook, not just a design statement.** §5.1
   asserts every bucket, bulkhead wait and suppression window takes the injected
   `clock`; T5's frozen-clock stepping and T9's refresh-error assertions depend on this
   being true everywhere, not just in the primitives under direct test. Recommend a
   lightweight check (grep-based CI check or a `code-reviewer` checklist item) that
   `internal/admission` and `internal/httpserver/webhook_admission.go` contain no bare
   `time.Now()` / `time.Sleep()` outside the clock abstraction and the one documented
   real deadline from item 1.

**Not blocking, noted for the record:** T6a/b/c, T14 and T16's five listed mutations are
adequate and well-targeted (each pins a specific invariant to a specific test failure).
Tenant-isolation and idempotency coverage (T1, T2, T6, T13) is sound. This verdict
applies to the test plan as written; it does not constitute sign-off on PRH-I4's
eventual implementation, which requires its own QA gate review against §11 as amended
by items 1–6 above, plus `security`'s review per §15.

## 17. Devops review

Verified at `24cbde1` against `cmd/platform-api/main.go`, `internal/httpserver/health.go`,
`internal/httpserver/middleware.go`, `internal/config/config.go`, and `deploy/aws/`.

**HTTP-TIMEOUTS-1 vs current state.** Confirmed: `main.go` sets only
`ReadHeaderTimeout: 5s` (no `ReadTimeout`, `WriteTimeout`, `IdleTimeout`). No handler in
`internal/httpserver` uses `http.Flusher`, SSE, or chunked streaming responses — admin/report
endpoints (`casino_reconciliation_handlers.go`, `casino_statement_admin_integration_test.go`,
etc.) are bounded JSON responses, not long-lived streams. This ADR's `BodyReadTimeout` (10 s,
via `SetReadDeadline` on webhook routes only) is compatible with those endpoints since it is
scoped per-request via `ResponseController`, not a global `http.Server.ReadTimeout` change —
correctly deferred as a separate, platform-wide fix (HTTP-TIMEOUTS-1) rather than bundled here,
since a global `WriteTimeout` sized for webhooks (10s) would need separate headroom analysis
against the slowest admin/report handler before being applied platform-wide. No objection to
sequencing; flag as a condition to keep the two changes in separate diffs (Condition 1).

**Config/env design and startup validation.** §9.3's fail-closed startup validation (rate/burst
bounds, `W_db < N`, `production` cannot disable) matches the existing `Load()` pattern in
`internal/config/config.go` (e.g. the provider-credential fingerprint key and Environment
validation already fail startup on bad config). `WEBHOOK_ADMISSION_OVERRIDES` as env-var JSON
is consistent with "no plaintext credentials" (it carries no secrets — only rates/bursts/ids)
and is explicitly scoped as operator config, not tenant config. Acceptable as designed. No
Vault/KMS involvement needed since nothing here is a credential.

**Tenant-slug directory refresher — lifecycle, `/readyz`, DB-down-at-boot.** §7's table is
correct: startup blocks on the first synchronous load, `/readyz` is not-ready until loaded, and
webhook routes 503 rather than run with unknown keying. This composes correctly with the
existing `readyzHandler` (`health.go`), which already gates on `db.HealthCheck` — if the DB is
down at boot, `/readyz` stays not-ready for the existing reason *and* the new one, so ECS never
routes traffic to a task that can't build the directory. One gap: the ADR does not say what the
refresher goroutine does on shutdown. `main.go`'s existing shutdown path
(`http.Server.Shutdown`) should cancel the refresher via the same context so it doesn't leak or
log after the logger/DB pool is torn down (Condition 2).

**Per-process limits under single-task ECS.** `deploy/aws/modules/ecs/variables.tf` defaults
`platform_api_desired_count` to 2 in the persistent environment (staging's ephemeral root
defaults to 1). §10 is honest that A2/A3/B1 are per-process and only state that protects
per-process resources (goroutines, memory, that process's pool) is authoritative — this is
correct today at desired_count=1, and remains correct (just with 2x effective budget, disclosed
already) once staging exercises 2 replicas for the ADR-0086 acceptance test. No Terraform
change is required by this ADR, and none should be made — confirmed the implementation
breakdown (§13) touches no `deploy/` file, matching the constraint that AWS/infrastructure
stays unchanged for this control.

**Observability — metric label cardinality.** All label sets in §8 are bounded: `domain` (3
fixed values), `tier` (6 fixed values), `outcome`/`status` are enums, and `provider_key` is
drawn from the finite process-global adapter registry (never raw input). The ADR explicitly
avoids a `tenant_id` metric label ("Per-tenant detail goes only to logs") — correct, since
`tenant_id` cardinality is unbounded over the platform's life; per-tenant detail belongs in
logs (bounded by suppression, §8) rather than metrics. Gauges (`webhook_inflight`,
`webhook_db_gate_in_use`, `webhook_limiter_keys{tier}`, directory size/age) are all
low-cardinality. No condition needed here — this is the correct pattern and should be the
template for future per-tenant metrics.

**Access-log path redaction (RL-F4).** Confirmed the defect: `middleware.go` line 75 (and the
panic-recovery line 94) logs `r.URL.Path` verbatim for every route, including the three
attacker-reachable webhook routes. The ADR's fix — `r.Pattern` on webhook routes instead of
`r.URL.Path` — is the right minimal fix, but as written in §13 it is scoped to "webhook routes"
inside `middleware.go`; the implementer should confirm this is done by checking the route
pattern (not by special-casing path prefixes, which is easy to bypass with a crafted path) and
that the panic-recovery logging line (94) gets the same treatment, since a panic mid-request is
exactly when an attacker-chosen path is most likely to be present (Condition 3).

**Verdict: APPROVE WITH CONDITIONS**

1. Land HTTP-TIMEOUTS-1 (platform-wide `ReadTimeout`/`WriteTimeout`/`IdleTimeout`) as a
   separate diff/ADR from PRH-I4, sized with headroom against the slowest existing admin/report
   handler — do not fold it into the webhook-admission change set.
2. The tenant-slug directory refresher goroutine must be wired to the same shutdown context as
   `main.go`'s `http.Server.Shutdown`, so it stops cleanly and does not log after the logger or
   DB pool is torn down; add a test asserting the goroutine exits on shutdown signal.
3. The RL-F4 access-log fix must key off the matched route pattern (not path-prefix matching)
   and must also cover the panic-recovery log line (`middleware.go` line 94), not only the
   happy-path `http_request` line.
4. No Terraform or `deploy/` changes are authorized or required for PRH-I4; if a future need
   arises (e.g., WEBHOOK-EDGE-1's source-IP allow-list, or a shared limiter store per §10's
   WEBHOOK-RL-SHARED-1), that requires its own ADR and devops review — do not introduce
   infrastructure changes under this ADR's implementation ticket.

## 18. Ledger-finance review

**Reviewer:** `ledger-finance`. **Scope:** §3 ORD-3, §5.2 B2, §6.4, T2/T6 only. I checked
these against the code at `dcddb2b`: `casino_handlers.go` and `deposit_handlers.go` webhook
paths, `casino/rejections.go`, the `casino.postBet`/`postWin`/`postRollback` tombstone gates,
`payments.postDepositReversalTombstone`, and `ledger/lockorder.go`.

**Findings**

- **Zero side effects holds as designed.** Every A/B rejection happens before
  `deps.DB.WithTenant`, so there is no ledger row, tombstone, projection change or intent
  transition. The only post-domain writes today are `recordCasinoCallbackRejection` and
  `payments.RecordDepositReversalRejection`. They run in separate transactions, they depend on
  error type, and nothing on the posting path reads them (only reconciliation does). Even so,
  admission must never reach them (C1).
- **Retry-after-429 is a first delivery.** Idempotency still comes from the DB unique keys
  (`(provider_id, provider_tx_id)`, `tombstone:<provider>:<ref>`) plus the payload-mismatch
  checks. Admission adds no state that the domain transaction consults.
- **B2 and ADR 0082.** B2 is a goroutine-only semaphore acquired before the pool acquire. It
  changes *arrival* order only. That is the same class of reordering a provider's own
  network/retries already produce. ADR 0082 lock order is set inside each transaction by
  `LockProjectionsForPosting` and is not affected. B2 adds no new wait-for edge, provided each
  request acquires one slot and never a second one while holding it (C1). A slot holder that
  is blocked on a row lock waits on another slot holder, and that holder can still make
  progress.
- **Rollback-before-bet is safe.** The rollback writes a tombstone. The retried bet hits
  `isProviderTxTombstoned`, gets a 409 `ErrOriginalTombstoned`, posts nothing and gets a
  rejection record. The player is never debited, so the money outcome is correct. The
  payments analogue (reversal-before-deposit, `postDepositReversalTombstone`) is equally safe.
- **§6.4 lists one reorder but omits one.** A win admitted before its limited bet returns
  `ErrBetNotFound` (4xx). It posts nothing and writes a rejection record, but it only recovers
  if the provider redelivers a 4xx. If it doesn't, the player is underpaid.

**Verdict: SIGN-OFF WITH CONDITIONS**

1. **C1.** An admission rejection (B1/B2, including a limiter panic) returns before
   `recordCasinoCallbackRejection` and the payment denial-audit branch. It is never wrapped
   as `CallbackRejectedError` or `DepositAlreadyReversedError`. T2/T6 assert zero rows in
   `casino_callback_rejections` and zero denial-audit rows for limited requests. B2 is
   acquired exactly once per request, strictly before `WithTenant`, and held (never
   re-acquired) through any follow-up rejection-record transaction. That means no 429/503 can
   follow a commit (ORD-3).
2. **C2.** Amend §6.4 to list win-before-bet and payments reversal-before-deposit. Extend T6:
   - (c) the late bet gets a 409 with zero ledger and projection change, and the tombstone is
     unchanged;
   - (d) a win before its bet gets a 4xx with zero postings. After the bet posts, the
     redelivered win posts exactly once, and the earlier rejection record does not block it;
   - (e) a deposit reversal retried after a 429 is idempotent on redelivery;
   - every variant asserts `SUM(debits)==SUM(credits)` and projection == rebuild.
3. **C3.** The §6.3 LF-C1 (b) reconciliation backstop must also surface
   provider-settled / platform-*rejected* events: tombstoned late originals and
   `bet_not_found` wins, not only unposted ones. This matters for any non-MOCK adapter whose
   `RetryWindow` or 4xx-retry behaviour is unknown.
4. **C4.** Nothing in the admission layer may be called from `internal/ledger`, `wallet`, or
   inside a domain transaction, and no code may rely on B2 wait order for correctness. The T16
   mutation "move B1/B2 inside `WithTenant`" stays mandatory.

This is sign-off on the financial design only. PRH-I4's diff needs its own ledger-finance
review before it is marked `IMPLEMENTED`.

## 19. Payments review

**Reviewer:** `payments`. **Scope:** section 9.1 payments burst defaults vs. real PSP callback
patterns, section 6.3 retry/status-code mechanism, section 6.4 idempotency, B2 (per-tenant
domain-tx cap = 3) vs. deposit/reversal callback throughput, and the ADR 0095 adapter-manifest
dependency for `WebhookRetrySemantics`.

**Burst defaults (9.1).** No real vendor callback pattern is asserted here, and none should be
invented -- the ADR itself labels 50/s pre-auth / 200 burst and 25/s verified / 100 burst as
technical, reversible defaults (`N=10` pool sizing), not vendor volumes, and commits to
re-measurement before any real-provider gate. That framing is correct and matches CLAUDE.md's
mocks-until-contract rule. The mechanism, not the numbers, is what a payments sign-off can
actually judge: GCRA-with-burst (5.1) plus per-`(domain, provider_id[, tenant])` overrides
(9.2) means a specific PSP's known replay-after-outage behavior (once a vendor contract
exists) is accommodated by raising that provider's override, not by a platform-wide change --
this is the right shape. 6.3's adapter-declared 429-vs-503 answer (undeclared non-MOCK adapter
fails registration; no-429-retry adapter gets 503 instead) is a sound fail-closed design that
does not depend on which vendor eventually shows up.

**Idempotency (6.4, ORD-3).** Correct: every rejection precedes `WithTenant`, so a limited
callback writes nothing and a redelivery is processed as a first delivery; the existing
`(provider_id, provider_tx_id)` constraint and payload-mismatch check are unchanged and still
the source of truth for true duplicates. This composes correctly with the ledger idempotency
invariant this domain must uphold on every posting.

**B2 cap (3) vs. deposit callback throughput.** At B1 = 25/s burst 100 per (tenant, provider),
a genuine backlog replay after a PSP-side outage can present 100 near-simultaneous verified
deposit/reversal callbacks to one tenant, but B2 admits only 3 concurrent domain transactions
for that tenant (waiting up to `DomainWait`=2s, then 503). Whether that queues harmlessly or
visibly delays legitimate deposit confirmations depends entirely on domain-tx duration, which
9.1 estimates at ms-scale only for the *pre-verification* reads -- it does not state a duration
estimate for the *domain* transaction (ledger posting, reserve accounting) that runs under B2.
This gap should be closed with data, not left as a documented assumption.

**ADR 0095 dependency (adapter manifest).** Agreed this belongs in the ADR 0095 capability
manifest as a first-class, mandatory field for every non-MOCK PSP adapter, not an optional one
-- a payments adapter without a declared `WebhookRetrySemantics` must fail registration exactly
as 6.3 specifies, both before and after 0095 lands.

**Verdict: APPROVE WITH CONDITIONS**

1. Before PRH-I4 enables the payments A3/B1/B2 defaults against any non-MOCK adapter, run the
   9.1 non-gating benchmark with a domain-transaction duration measurement for deposit *and*
   reversal postings (not just the pre-verification read estimate), and add a payments-specific
   scenario to section 11 (e.g. folded into T19) that drives >= B1-burst concurrent verified
   deposit callbacks for one tenant and confirms B2's 2s `DomainWait` does not silently convert
   legitimate backlog replay into sustained 503s within a typical PSP retry window.
2. `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter, RetryWindow}` must be a
   **mandatory, fail-closed** field in the ADR 0095 adapter capability manifest for every
   non-MOCK payments adapter -- payments will supply this declaration per PSP only once a
   vendor contract is confirmed; until then, and until 0095 lands, the interim per-provider
   config key must enforce the same fail-closed registration behavior described in 6.3.
3. Extend T6a (`RetryAfter429_Idempotent`, payments deposit) to an explicit reversal/refund
   variant, or confirm one already exists elsewhere in the payments suite -- reversals share
   the `(provider_id, provider_tx_id)` idempotency path and are payments-critical, and should
   not rely on the deposit case alone to pin ORD-3/idempotency for that code path.
4. Any code path in the eventual PRH-I4 diff that posts a ledger entry (the `D` step, domain
   processing) requires `ledger-finance` review per CLAUDE.md, independent of this admission
   review -- this ADR's 6.4/idempotency framing does not substitute for that review.

## 20. Architect review

**Reviewer:** `architect`. **Scope:** cross-domain boundaries, ADR 0094/0091/0022 consistency,
tenant directory, config ownership, isolation-tightening path, ADR 0095 interface. Verified at
`dcddb2b`. ADR 0095 is not yet in the repo at this commit; item AC6 states what it must provide.
Editorial: there are two `## 16` headings (QA, devops); renumber devops to §17 when accepted.

**Verdict: APPROVE WITH CONDITIONS**

**AC1 — `internal/admission` boundary.** Accepted as a stdlib-only leaf (no cycles possible).
Allowed importers: `internal/httpserver` (and its tests) only. It must not be imported by
`webhookauth`, `payments`, `casino`, `kyc`, `ledger`, `db`, `identity` or `config`: admission is a
transport-layer control and domain packages stay unaware of it. `internal/config` produces plain
values; `httpserver` maps them to admission types. PRH-I4 adds an import-guard test (same style
as the `db` raw-guard test) that pins both directions. No change to any domain package's public
API or to `webhookauth.TenantReader` is authorized under this ADR.

**AC2 — ADR 0094 consistency (INV-POOL, txscope).** Consistent: A4b wraps only
`GetTenantBySlug` (a `WithoutTenant` read) and the `WithTenantReadOnly` calls, and
`gatedReader` delegates to `*db.Pool`, so txscope marking is unchanged. Conditions:
(a) the gate is held exactly for the duration of one `WithTenantReadOnly`/`GetTenantBySlug`
call, acquired before and released after it returns, never across the Fetcher;
(b) `gatedReader` is non-reentrant: if `txscope.Held(ctx)` is true, or the request already holds
an A4b slot, it fails closed (503) instead of acquiring again, since nested acquire at per-key
cap 2 is a hold-and-wait self-deadlock; add a unit test and a T16 mutation for it;
(c) the ADR 0022 §3 point-9 statement set and uniform 401 (ADR 0091) are unchanged, as §5.3/§6.2 state.

**AC3 — tenant-slug directory.** Accepted as **non-authoritative**. It is a limiter-key hint only.
Conditions: it stays unexported in `httpserver`, exposes only `Contains(slug) bool` (no tenant id,
status or licensing model), and is used by no other route or for tenant resolution (staff login,
player routes). `code-reviewer` checks this. `identity.ListActiveTenantSlugs`'s doc comment states it
is a platform-scope catalogue read that must never be used for authorization.
Authority stays with `GetTenantBySlug` + active check + `VerifyCallback`, and the trusted binding is
only the `VerifiedCallback`.

**AC4 — isolation-tightening path.** Keying B1/B2 on `tenant_id` survives schema/db/cluster-per-tenant.
Two constraints recorded for the path:
(i) the directory and `GetTenantBySlug` both assume a platform-scope `tenants` catalogue. Under
db-per-tenant, that catalogue must stay in a control-plane store, not move into tenant databases.
(ii) A4b/B2 caps and §9.3's `W_db < N` / `B2 < N` are defined against one shared pool. When pools
become per tenant, the caps are computed per routed pool. Not built now; add one line to §10.

**AC5 — config ownership.** Agree with §9.2: this is platform-operator configuration, not
brand/tenant configuration. It is owned by `security` (values) and `devops` (delivery) through
`internal/config`, and it is never partner-console editable. Recommendation (non-blocking): key
tenant overrides by `tenant_id` for both tiers, mapping to slug via the directory for A3. Slug
renames then cannot silently detach an override.

**AC6 — interface ADR 0095 must provide (blocking for ACCEPTED status of any non-MOCK webhook
adapter, not for PRH-I4).**
- `WebhookRetrySemantics{Retries429, Retries503, HonorsRetryAfter bool; RetryWindow time.Duration}`
  is a static, per-adapter declaration in the capability manifest. It is a vendor property: no DB,
  no per-tenant variance, and tenant config may never relax it.
- The type lives in the manifest/`webhookauth`-level package, not in `admission` or `httpserver`.
  It is exposed by the domain orchestrator's existing process-global registry beside
  `WebhookScheme(providerID)`, e.g. `WebhookRetrySemantics(providerID) (WebhookRetrySemantics, bool)`.
  `httpserver` maps it to the B1 status (§6.3).
- MOCK vs non-MOCK is decided by the `providerkind.Synthetic` / `ProductionEligible` markers, never
  by name or config. A non-`Synthetic` webhook adapter without a declaration fails registration at
  startup, alongside `RefuseSyntheticInProduction`.
- The §6.3 interim "per-provider config key" is **rejected** as a second source of truth for a vendor
  property. No `ProductionEligible` adapter exists today, so PRH-I4 ships MOCK behaviour plus the
  fail-closed registration check, and ADR 0095 supplies the manifest field.
- `RetryWindow` feeds alerting (§8) only, never admission decisions.

**AC7 — R5 scope.** Accepted as out of scope. The orchestrator should register the
authenticated-route pool-pinning class (RL-F2 for player/admin routes) as a separate registry item
rather than leave it only in §12.

This verdict covers architecture only. It does not replace `security` (owner), `payments` (§6.3),
`ledger-finance` (§6.4/T6) concurrence, or the security review of the PRH-I4 diff.
