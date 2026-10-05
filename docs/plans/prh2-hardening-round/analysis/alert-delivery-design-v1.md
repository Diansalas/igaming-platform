# ALERT-DELIVERY-1 continuation: delivery activation design (migration 0117)

- Author: architect. Date: 2026-10-05. Base: `95b17c5` (read-only inspection; nothing built, no tests run).
- Status: **DESIGN / PROPOSED.** To be recorded as an amendment to ADR 0102 (new §18, "Delivery
  activation"), then reviewed by `security` (hard gate: route authoring, secret refs, RLS),
  `ledger-finance` (post-commit guarantee), `product-owner-proxy` (scope), and `qa` (matrix §10).
- Outcome when implemented: routing configuration, activation gating, channel contract, MOCK channel,
  readiness signal and audit are **IMPLEMENTED**. Real channel: **NOT IMPLEMENTED / PROVIDER
  DEPENDENT**. Recipients: **BLOCKED on HD-PRH2-4-OPS**. **ALERT-DELIVERY-1 stays OPEN.**

---

## 1. Inventory: what exists today (verified at `95b17c5`)

### 1.1 Data model (migration `migrations/0110_durable_alerting.up.sql`)
| Object | Lines | What it does |
|---|---|---|
| `alert_kinds` | 121-216 | Immutable, migration-seeded vocabulary: `severity` p1/p2/p3, `scope`, `simulation`, `requires_subject`, `allowed_keys`. CHECK at 141-144 allows `requires_subject=false` ONLY for the three meta-Kinds. 0114 added `kyc.submission_failed_terminal` via a temporary literal single-Kind INSERT policy (`0114...up.sql:606-625`): the precedent 0117 reuses |
| `alerts` | 221-405 | One row per open dedup condition. `state` open/acked/resolved (242). State guard 346-397: transitions open→acked, open→resolved, acked→resolved; actor forced from `alerting_validated_platform_admin()` (33-45); reason required on resolve |
| `alert_occurrences` | 410-459 | Append-only; `raised_by_scope` forced from GUCs |
| `alert_routes` | 464-547 | `(scope, tenant_id, severity, escalation_step) -> channel_kind + recipient_ref`, `escalate_after`, one-way supersession. `CHECK (tenant_id IS NULL)` (471); `channel_kind CHECK IN ('log','mock')` (474); `recipient_ref NOT NULL` + opaque-shape CHECKs (477-478). **No enable flag, no channel config, no kind key, no secret ref. Never seeded.** |
| `alert_deliveries` | 552-616 | Append-only events `claimed/sent/failed/unrouted/dead/suppressed_simulation`; `last_error_class` enum `timeout/unavailable/rejected/misconfigured/unknown` (565); UNIQUE `(alert_id, step, attempt_no, event)` (572); unrouted pinned to `attempt_no=0` (562) |
| RLS | 622-937 | ENABLE+FORCE; families tenant-owned, subject-read, subject-raise, platform-admin (FOR ALL on routes, 812-831), dispatcher (SELECT routes, INSERT deliveries, meta-only alert INSERT 862-900). Common exclusion set on every predicate |

### 1.2 Go (`internal/alerting`)
| Item | Location | Notes |
|---|---|---|
| `ChannelKind` log/mock | `dispatcher.go:24-29` | |
| `ErrorClass` (mirrors DB enum) | `dispatcher.go:41-50` | No retryable/permanent split |
| `Delivery` (IdempotencyKey per attempt, **DedupKey `<alert_id>:<step>`**) | `dispatcher.go:55-71` | |
| `Sink` interface `ChannelKind()` + `Deliver(ctx, Delivery) (Outcome, ErrorClass)` | `dispatcher.go:77-80` | No recipient-validation hook, no credential |
| `LogSink` (never fails; logs `recipient_ref`) | `dispatcher.go:85-105` | Log line ≠ human notification |
| `MockSink` (Synthetic-marked, `DeliverFunc`, `Attempts` slice **without a mutex**) | `dispatcher.go:112-138` | |
| `DispatcherConfig` MaxAttempts=5, backoff 1s→60s cap, ClaimLease 2m | `dispatcher.go:143-193` | Same budget for every severity |
| `RunOnce`: read due work (short tx) → Deliver with no tx (AL-5, `txscope.Held` guard 470-473, timeout ClaimLease/2 483) → record outcome | `dispatcher.go:262-285, 415-503` | |
| Due-work classification (absent/unrouted/failed/sent+escalation/stale claimed/dead terminal) | `dispatcher.go:306-402` | `dead` is terminal at every severity (394) |
| **No-sink case: log line only, no row, no metric (N-4 gap)** | `dispatcher.go:437-445` | |
| `resolveRoute` (severity+step only, `scope='platform'`, latest `effective_from`) | `dispatcher_actions.go:35-56` | No kind key, no enable flag |
| Claim via UNIQUE insert | `dispatcher_actions.go:64-82` | |
| Retry vs dead (every class retried; dead after MaxAttempts) | `dispatcher_actions.go:102-145` | Permanent errors burn the whole budget |
| Unrouted marking (RETURNING-gated meta) | `dispatcher_actions.go:147-170` | |
| Meta non-recursion (`IsMetaKind`) | `dispatcher_actions.go:190-204`, `kind.go:243` | |
| Loop (immediate pass, panic recovery, drain) | `dispatcher_loop.go:60-141` | |
| Metrics: `alert_raise_failures_total`, `alert_unrouted_total{severity}`, `alert_dead_total{channel_kind}`, `alert_stale_claims_total`, `alert_dispatcher_passes_total{result}` | `metrics.go` | No readiness, no send-latency, no per-result attempt counter |

### 1.3 Wiring, HTTP, auth
| Item | Location |
|---|---|
| Dispatcher started, **LogSink only**, interval 15s constant | `cmd/platform-api/main.go:492-497`; drain `:646-655`; pinned by `cmd/platform-api/alert_dispatcher_wiring_test.go` |
| MOCK-ADAPTER-PROD-1 guard (Synthetic/ProductionEligible markers); **the alert sink is not registered** (N-5) | `internal/providerkind/guard.go:58`; `cmd/platform-api/registrations.go:372-431` |
| Ack/resolve `POST /v1/admin/alerts/{alertID}/ack|resolve`, `auth.PermAlertManage`, explicit platform-scope check, `FOR NO KEY UPDATE`, audit in same tx with before/after state in metadata, IP, UA, request id, `SubjectTenantID` | `internal/httpserver/alert_admin_handlers.go:41-49, 77, 82-183`; permission `internal/auth/permission.go:541-553` |
| No list/status endpoint; no route/channel write endpoint; refused ack/resolve not audited; ack takes no reason code | same file (absence) |
| `/readyz` = DB + webhook directory only | `internal/httpserver/health.go:37-56` |
| Secret-store refs (`awssm://` ARN+versionId, `devfile://`, `memory://`), strict `ParseRef`, namespace rule, `Fetcher.Fetch(ctx, tenantID, ref, fingerprint)` refuses when a tx is held | `internal/secretstore/secretstore.go:171-237`, `fetcher.go:228` |

### 1.4 Registry state
`docs/governance/task-registry.md` row `PRH-2-IWIRE-MERGE-STATE` (line 4125): ALERT-DELIVERY-1 closes only when
(a) real human-authored routes, (b) a real channel that is neither log nor MOCK, (c) a named human
recipient/on-call, (d) ADR 0102 §17.7 preconditions: new K2 breach pages; ALERT-RETENTION-1 + volume
bound; adapter dedupes on `DedupKey` (real-adapter test) and honours ctx; SR-7 route authoring + its
security review (N-4: refuse `mock` in prod / missing sink visible); Vault/KMS credentials, no body
logging; escaped attribute rendering. Follow-ups there: S-5, refused ack/resolve not audited, ack
reason code, N-5 (production refusal of synthetic sinks in `NewDispatcher`).

## 2. Gap list (requirement → gap)

| # | Requirement | Gap today |
|---|---|---|
| G1 | Provider/channel-neutral contract | `Sink` has no recipient validation, no credential input, no retryable/permanent split, no receipt; conformance for a future real adapter is untestable |
| G2 | Explicit routing configuration | Routes keyed only by (severity, step); no kind key; no channel configuration object; no authoring API (raw SQL only) |
| G3 | Fail-closed activation | No `enabled` flag: any inserted route is live; nothing stops a route without a usable channel/credential; a route to an unwired kind black-holes (N-4) |
| G4 | Channel credentials by secret NAME | No column; nothing ties a channel to the secret store; nothing prevents pointing at a tenant PSP credential |
| G5 | Severity support | Same retry budget for all severities; `dead` is terminal even for p1 when a step-n+1 route exists |
| G6 | Ack/resolve semantics + audit | Exists; refused attempts unaudited; ack has no reason; no read surface showing delivery state |
| G7 | Delivery-failed visibility | `dead` only as a delivery row + meta-alert; no derived per-alert delivery state; meta-alert travels over the same (possibly dead) channel |
| G8 | Tenant/platform scope | Routes are platform-only (`CHECK tenant_id IS NULL`); fine for today's Kinds, but no stated rule for tenant recipients |
| G9 | Visible not-ready signal | No signal at all that no p1/p2 route exists other than `unrouted` counters |
| G10 | MOCK/test channel | `MockSink` exists but is not concurrency-safe, has no scripted failure sequence, no dedupe assertion, no conformance suite; it may never be wired in any binary (pinned) |
| G11 | Production safety of sinks | Alert sinks not in `buildRegistrations` (N-5); `mock` route not refused in production |
| G12 | Route-change governance (SR-7) | Neither four-eyes nor `alerting.route_changed` exists; last p1 route can be superseded |

## 3. Design overview

One workstream, one migration (0117), one backend engineer:

1. **Migration 0117**: `alert_channel_kinds` (reference), `alert_channels` (versioned channel config with secret
   ref NAME), `alert_routes` extended (kind key, channel key, `enabled DEFAULT false`, activation CHECK +
   trigger, last-route protection), `alert_deliveries` extended (unrouted reason, receipt fingerprint),
   one new Kind `alerting.route_changed`, view `alert_delivery_status`. **No channel, route or recipient
   rows are seeded.**
2. **Go**: `Channel` replaces `Sink`; retryable/permanent classification; severity budgets; escalate-on-dead
   for p1; `no_sink`/`channel_disabled`/`channel_not_eligible` become visible unrouted rows; readiness
   evaluation + gauge; `MockChannel` + conformance suite in a test-support package.
3. **HTTP**: platform-admin route/channel authoring (versioned, audited, reason-coded, raises
   `alerting.route_changed`), status + list read endpoints, ack reason + refusal audit.
4. **Wiring**: alert channels registered with the MOCK-ADAPTER-PROD-1 guard; MOCK channel only behind a
   dev gate refused in production; readiness mode env.

Not in this workstream: any real channel adapter, any recipient, ALERT-RETENTION-1, the K2 per-tx
paging change (§12).

## 4. Data model: migration 0117

### 4.1 `alert_channel_kinds` (immutable reference, migration-written only; same pattern as `alert_kinds`)
| Column | Type | Notes |
|---|---|---|
| `channel_kind` | TEXT PK | `^[a-z][a-z0-9_]{1,31}$` |
| `human_notification` | BOOLEAN NOT NULL | **true only for a channel that reaches a person.** `log` = false, `mock` = false |
| `synthetic` | BOOLEAN NOT NULL | `mock` = true |
| `requires_credential` | BOOLEAN NOT NULL | log/mock = false |

Seed exactly: `('log', false, false, false)`, `('mock', false, true, false)`. **No real kind is seeded.**
A real kind arrives with its adapter in a later migration (number allocated then). This makes "log output is
not human notification" structural and lets readiness be computed in SQL.

0117 replaces the `channel_kind CHECK IN ('log','mock')` on `alert_routes` (0110:474) and
`alert_deliveries` (0110:564) with FKs to this table (DROP CONSTRAINT / ADD FOREIGN KEY; DDL does not fire the
append-only triggers). Deny UPDATE/DELETE/TRUNCATE triggers; FORCE RLS; one `FOR SELECT USING (true)` policy.

### 4.2 `alert_channels` (platform-only, append-only, one-way supersession like `alert_routes`)
| Column | Type / CHECK | Notes |
|---|---|---|
| `id` | UUID PK | |
| `channel_key` | TEXT `^[a-z0-9][a-z0-9_.-]{0,63}$` | Stable logical name; routes reference this, so a credential rotation (new version) does not touch routes |
| `channel_kind` | TEXT FK `alert_channel_kinds` | |
| `tenant_id` | UUID NULL, `CHECK (tenant_id IS NULL)` | Platform-only in 0117 (§8) |
| `credential_secret_ref` | TEXT NULL; ≤512 bytes; no control chars/space; `^(awssm|devfile|memory)://`; **must contain `/platform-alerting/` and must NOT contain `/provider-creds/`** | A secret-store **handle (name + pinned version)**, never a value. The namespace rule stops a channel from being pointed at a tenant PSP/KYC credential (confused deputy) |
| `credential_fingerprint` | TEXT NULL | `CHECK ((credential_secret_ref IS NULL) = (credential_fingerprint IS NULL))`; keyed fingerprint computed server-side at write time (providercred pattern) |
| `enabled` | BOOLEAN NOT NULL DEFAULT false | |
| `reason_code` | TEXT NOT NULL `^[a-z0-9_]{1,64}$` | Why this version exists |
| `effective_from`, `superseded_at`, `superseded_by` | | As routes; `superseded_by` FK **DEFERRABLE INITIALLY DEFERRED** so supersede-and-insert fits one tx |
| `created_by` | UUID NOT NULL | Forced by trigger from `alerting_validated_platform_admin()` |
| `created_at` | | Forced |

- UNIQUE `(channel_key) WHERE superseded_at IS NULL` (one current version per key).
- Guard trigger: immutable columns; one-way supersession; **`enabled ⇒ (NOT kind.requires_credential OR
  credential_secret_ref IS NOT NULL)`**; refuse disabling/superseding the current version of a channel that
  is referenced by the **last** enabled human-notification p1 route (§4.3 rule R5) unless the replacement
  version is itself enabled.
- RLS (common exclusion set): `alerts_platform_admin` SELECT+INSERT+UPDATE(supersession only);
  `alerts_platform_service_dispatcher` SELECT. **No tenant policy at all** (tenant sessions see 0 rows).
- Grants: `SELECT, INSERT, UPDATE` to `igaming_runtime` (RLS binds).

### 4.3 `alert_routes` (ALTER, keep table and its history)
New columns:
| Column | Notes |
|---|---|
| `kind TEXT NULL REFERENCES alert_kinds(kind)` | NULL = severity-default route; non-NULL = kind-specific override. If set, trigger forces `severity` from `alert_kinds`; simulation Kinds refused |
| `channel_key TEXT NULL` | Logical reference to `alert_channels.channel_key` (validated by trigger, not FK, because versions rotate) |
| `enabled BOOLEAN NOT NULL DEFAULT false` | Existing rows (dev/test only; prod has none) become disabled: fail-closed |
| `reason_code TEXT NULL` | Required (trigger) on every row inserted after 0117 |

Changed: `recipient_ref` DROP NOT NULL (a disabled route may be staged before the recipient is known);
`channel_kind` DROP NOT NULL and forced by trigger from the current channel version when `channel_key` is set.

Rules (DB-enforced):
- **R1** `CHECK (NOT enabled OR (channel_key IS NOT NULL AND recipient_ref IS NOT NULL))`.
- **R2** trigger on INSERT with `enabled`: the current `alert_channels` version for `channel_key` exists and is
  `enabled`; if its kind `requires_credential`, the version has a secret ref + fingerprint.
- **R3** UNIQUE NULLS NOT DISTINCT `(scope, tenant_id, kind, severity, escalation_step) WHERE superseded_at IS NULL`
  (at most one current route per key; replaces "latest effective_from wins"). `superseded_by` FK altered to
  DEFERRABLE INITIALLY DEFERRED.
- **R4** `recipient_ref` keeps 0110's opaque-shape CHECKs (no `@`, no phone shape). **No row is seeded; no
  migration, test fixture outside tests, or code path may insert a recipient.**
- **R5 (SR-7 ii)** a statement-level/row trigger refuses any supersession that leaves zero current, enabled
  routes with `severity='p1' AND escalation_step=0` whose channel kind is `human_notification`, **once at
  least one has existed**. Disabling = superseding with an `enabled=false` version, so R5 covers both.
- `CHECK (tenant_id IS NULL)` stays (§8).

Activation is therefore always an append: a new version with `enabled=true`, which is audited and raises
`alerting.route_changed` (§7.2). No in-place flag flip exists.

### 4.4 `alert_deliveries` (ALTER)
- `unrouted_reason TEXT NULL CHECK IN ('no_route','channel_disabled','no_sink','channel_not_eligible')`
  with `CHECK (unrouted_reason IS NULL OR event = 'unrouted')`. Pre-0117 `unrouted` rows keep NULL (no
  backfill: the table is append-only); Go always writes a reason from 0117 on, pinned by a test.
- `channel_key TEXT NULL` (which channel version family was used).
- `receipt_fp TEXT NULL CHECK (receipt_fp ~ '^[0-9a-f]{12}$') CHECK (receipt_fp IS NULL OR event = 'sent')`:
  `providerref.Fingerprint` of the vendor's message id (proof of hand-off without storing vendor ids).
- `channel_kind` CHECK → FK (§4.1).

### 4.5 New Kind `alerting.route_changed`
p2, platform, `requires_subject=false`, `in_tx_raisable_by_tenant=false`, `allowed_keys
{'object','action','severity','reason_code'}` (closed tokens), `raise_mode in_tx`. Inserted with the 0114
temporary literal single-Kind policy pattern. 0117 drops and re-adds the 0110:141-144 CHECK to include this
Kind in the `requires_subject=false` list. It is raised by the **platform-admin** session (existing
`alerts_platform_admin` family allows platform-owned inserts); the dispatcher's meta-only WITH CHECK
(exactly three Kinds, 0110:862-900) is **unchanged**. `IsMetaKind` is unchanged (route_changed is delivered
normally and is not part of the non-recursion set).

### 4.6 View `alert_delivery_status` (`WITH (security_invoker = true)`, so the caller's RLS applies)
Per alert: `alert_id, state, severity, kind, latest_event, escalation_step, attempt_no, delivery_state` where
`delivery_state` ∈ `pending` (no row / claimed), `retrying` (failed), `delivered` (sent),
`delivery_failed` (dead with no further step), `unrouted`, `suppressed` (simulation).

**Deviation from the instruction, stated:** the owner asked for alert state `delivery_failed`. This design
does **not** add it to `alerts.state`: `alerts.state` is the human workflow (open/acked/resolved) and the
dispatcher deliberately has no UPDATE on any alert table (AL-10, security-accepted). `delivery_failed` is a
**derived delivery state** (view + API + metric + `alerting.delivery_dead` meta-alert). Same visibility,
no new write privilege. If the owner insists on a stored state, that is an AL-10 change needing security.

### 4.7 Down migration
Refuse if `alert_channels` has any row or any `alert_routes` row has `channel_key`/`kind` set or any delivery
row has `unrouted_reason`/`receipt_fp`/`channel_key`; refuse if any `alerts`/`alert_occurrences` row has kind
`alerting.route_changed`. Otherwise drop view, columns, table, FKs; restore the 0110 CHECKs. Must succeed on
an empty scratch DB (chain-test convention).

## 5. Channel contract (replaces `Sink`; mechanical rename across tests)

```go
package alerting

type Channel interface {
    Kind() ChannelKind
    // ValidateRecipient checks the opaque ref's syntax for this channel. No I/O; never logs ref.
    ValidateRecipient(ref string) error
    // Send makes at most one human-visible notification per msg.DedupKey (adapter MUST dedupe on
    // DedupKey, never AttemptKey), honours ctx, never logs bodies or credentials, renders every
    // attribute as escaped data, and classifies every failure. Never called with a tx held (AL-5).
    Send(ctx context.Context, msg Message, to Recipient, cred Credential) SendResult
}

type Message struct {
    DedupKey        string // "<alert_id>:<step>"  (stable across attempts)
    AttemptKey      string // "<alert_id>:<step>:<attempt_no>" (logs/metrics only)
    AlertID         uuid.UUID
    Kind            Kind
    Severity        Severity
    Discriminator   string
    SubjectTenantID uuid.UUID // id only; no tenant name, no PII
    Attributes      map[string]AttrValue
    EscalationStep  int
    FirstSeenAt     time.Time
}
type Recipient struct{ Ref string }                        // opaque, from the route
type Credential struct{ ChannelKey string; Secret secretstore.Secret } // zero for log/mock
type SendResult struct {
    Outcome      Outcome    // sent | failed
    Class        ErrorClass // "" iff sent
    ReceiptRef   string     // vendor message id; dispatcher stores only its fingerprint
}

// Retryable: timeout, unavailable, unknown. Permanent: rejected, misconfigured.
func (c ErrorClass) Retryable() bool
```
- The `last_error_class` DB enum is unchanged (no migration churn); the split is a Go method plus a
  table-driven test pinning it.
- The dispatcher, not the channel, fetches the credential: `secretstore.Fetcher.Fetch(ctx, uuid.Nil, ref,
  fingerprint)` **after** the read tx closed and before `Send` (Fetch itself refuses a held tx). Fetch class
  mapping: `store_unavailable` → `unavailable` (retryable); `not_found/access_denied/integrity/
  invalid_ref/no_backend/store_config` → `misconfigured` (permanent).
- `LogChannel` (renamed `LogSink`): `Kind()=log`, `ValidateRecipient` accepts the 0110 shape, never fails,
  logs **no attributes** (today it logs `recipient_ref` and discriminator; keep those, they are ids), marked
  `ProductionEligible` (it is a legitimate record), and `alert_channel_kinds.human_notification=false`.

## 6. MOCK / test channel

`internal/alerting/alertingtest` (new package):
- `RecordingChannel` (`Kind()=mock`, `SyntheticComponent()` marker): mutex-protected record of
  `(Message, Recipient, Credential.ChannelKey)` per call; **scripted outcomes** (`Script([]SendResult)` consumed
  in order, then default sent); `FailWith(class)`; `BlockUntilCtxDone()` to simulate a hung vendor;
  `PanicOnce()`; built-in dedupe (a second `sent` for the same `DedupKey` returns sent without a second
  "notification" and increments `DuplicateSuppressed`), so tests can assert human-visible notifications ==
  distinct DedupKeys.
- `RunChannelConformance(t, factory)`: the suite any real adapter must pass before it can be merged:
  dedupe on DedupKey across retries; ctx cancellation returns `timeout` promptly; no secret/body in captured
  logs (slog handler capture); every failure classified; attributes containing markup-like or control
  characters rendered escaped (attributes are charset-restricted already; the suite feeds the widest legal
  values); `ValidateRecipient` refuses email/phone shapes. Run against `RecordingChannel` and `LogChannel`
  in this workstream.
- Import restriction: static test (like `TestSecretStore_MemstoreImportedOnlyByTests`) allows
  `alertingtest` only from `_test.go` files **and** `cmd/platform-api/devwiring_alerting.go` (dev gate below).
- Dev profile: `ALERT_MOCK_CHANNEL=enabled` wires `RecordingChannel` (records to an in-memory ring + one Info
  log per send, labelled `mock_alert_delivery_not_a_notification`). **Config validation refuses
  `ALERT_MOCK_CHANNEL=enabled` when `GuardEnvironment()=="production"`**, and the providerkind guard refuses it
  again (Synthetic). This changes the I-wire pin "MockSink is never wired in a binary"
  (`alert_dispatcher_wiring_test.go`) to "mock channel wired only behind the dev gate": **security sign-off
  required**; if security declines, drop the dev profile and keep the channel tests-only (no other item
  depends on it).
- Existing `MockSink` is folded into `RecordingChannel`; tests migrate.

## 7. Dispatcher semantics (changes to `dispatcher.go` / `dispatcher_actions.go`)

### 7.1 Route resolution
Per pass, cache by `(kind, severity, step)`. Query (dispatcher identity, injected `now`):
current routes `WHERE enabled AND superseded_at IS NULL AND effective_from <= $now AND scope='platform' AND
severity=$sev AND escalation_step=$step AND (kind=$kind OR kind IS NULL) ORDER BY kind NULLS LAST LIMIT 1`,
joined to the current enabled `alert_channels` version by `channel_key` and to `alert_channel_kinds`.
Outcomes → delivery row:
| Situation | Row |
|---|---|
| No enabled route | `unrouted` reason `no_route` (+ existing RETURNING-gated `alerting.unrouted` meta + metric) |
| Route but channel version missing/disabled | `unrouted` reason `channel_disabled` |
| Channel kind has no `Channel` wired in this binary (fixes N-4) | `unrouted` reason `no_sink` |
| Channel kind `synthetic` and `GuardEnvironment()=="production"` | `unrouted` reason `channel_not_eligible` |

An `unrouted` row is still one per (alert, step) (0110:562) and still re-evaluated each pass; the "already
unrouted, skip insert" shortcut (`dispatcher.go:426-432`) stays.

### 7.2 Retry, backoff, budgets (technical defaults, devops-reviewable; env §9)
- Permanent class (`rejected`, `misconfigured`) → `dead` immediately (no budget burn).
- Retryable → backoff `min(base·2^attempt, cap)` with ±20% jitter from an injectable rand (tests seed it),
  until the severity budget: p1 8 attempts / cap 5 min; p2 5 / 1 min; p3 3 / 1 min.
- Stale-claim lease and `Send` timeout (`ClaimLease/2`) unchanged.
- **Panic in `Send`**: recovered per alert inside `processOne`, recorded as `failed` class `unknown` (today a
  panic aborts the whole pass via `runPassRecovered`, starving every later alert in that pass; S-5-adjacent).

### 7.3 Terminal failure and meta-alert
- `dead` → derived `delivery_failed` (§4.6), `alert_dead_total{channel_kind}`, existing
  `alerting.delivery_dead` raise (dedup discriminator `severity:<p>`), non-recursion via `IsMetaKind`
  (unchanged; `alerting.route_changed` is not meta so it can raise `delivery_dead` when it dies, but
  `delivery_dead` dying raises nothing).
- **Loop protection stated**: meta-alerts can only fan in to three open rows (one per severity
  discriminator) and never recurse; a fully dead channel therefore ends at one dead meta-alert plus
  metrics. **The out-of-band path is metrics** (`alert_dead_total`, `alert_routing_ready`,
  `alert_channel_send_failures_total`) scraped by the external monitoring stack
  (`deploy/aws/modules/observability`) whose own notification target is an operator input (§12).

### 7.4 Escalation
- Existing: `sent` + route `escalate_after` → step n+1 when still `open` (not acked). Kept.
- New, **p1 only**: `dead` at step n → if a current enabled route exists for step n+1, step n+1 attempt 0 is due
  immediately (escalate on delivery failure). No step n+1 route → terminal (no extra `unrouted` row, to avoid
  noise). p2/p3: `dead` terminal.
- Escalation windows are route data (`escalate_after`), never code constants; values are operator input.

### 7.5 Severity behaviour summary
| | p1 | p2 | p3 |
|---|---|---|---|
| Retry budget | 8 / 5 min cap | 5 / 1 min | 3 / 1 min |
| Escalate on unacked (`escalate_after`) | yes | yes | yes (if configured) |
| Escalate on dead | yes | no | no |
| Counted for readiness | yes | yes | no |
| Unrouted meta | yes | yes | yes (unchanged) |

### 7.6 Dedupe
Unchanged keys: alert dedup `(tenant_id, dedup_key)` over non-resolved; delivery claim UNIQUE
`(alert_id, step, attempt_no, event)`; channel dedupe on `DedupKey = <alert_id>:<step>`. Result:
at-least-once hand-off, at-most-once human notification per (alert, step) **iff** the adapter honours
DedupKey (conformance suite).

### 7.7 Post-commit guarantee (ledger-finance)
No business transaction performs or awaits delivery: raise paths are unchanged (§7 of ADR 0102); the
dispatcher runs only under `alert_dispatcher`, which has no grant on any financial table; channel/secret
I/O happens with no tx held (AL-5, `txscope.Held` + Fetcher guard). A channel failure, panic, timeout or a
dead/unrouted outcome cannot roll back or block any financial write.

### 7.8 Readiness (fail-closed, visible, not silent)
- Each pass (and once at startup) the dispatcher evaluates, for p1 and p2: exists a current enabled route at
  step 0 (severity-default, `kind IS NULL`) whose channel is current+enabled, kind `human_notification=true`,
  not synthetic in production, and wired in this binary. Kind-specific routes do not satisfy it (coverage
  must be total for the severity).
- Exposed as gauge `alert_routing_ready{severity}` (0/1), Error log `alert_routing_not_ready{severity,reason}`
  on every transition and at most every 10 min while not ready, and `GET /v1/admin/alerting/status`.
- `/readyz` coupling via `ALERT_ROUTING_READINESS_MODE`:
  - `report` (default outside production): `/readyz` unaffected; signals above only.
  - `enforce`: `/readyz` returns 503 `alert routing not ready` from the cached last evaluation (no extra DB
    query on `/readyz`; not ready until the first evaluation completes).
  - **In production the variable has no default: startup refuses if unset.** Which value production uses is
    a human decision (§12 H5). Today, with no `human_notification` kind in the vocabulary, `enforce` can never
    be satisfied: that is the intended fail-closed outcome.

## 8. Tenant isolation

- Every alert today is platform-owned (subject-tenant pattern); routes and channels are platform-only
  (`CHECK tenant_id IS NULL`), RLS exposes `alert_channels`/`alert_routes` only to the validated platform admin
  and the dispatcher; tenant, tenant-principal, player, acting and mixed-GUC sessions see 0 rows.
- Subject tenants keep read-only access to their subject alerts and never see deliveries, routes, channels,
  recipient refs, or secret refs (0110 policies unchanged; the view is `security_invoker`).
- All new endpoints: `RequireStaffPrincipal` + permission + explicit `tc.TenantID == uuid.Nil` check
  (same two-layer pattern as `alert_admin_handlers.go:96-102`), and the DB family as third layer.
- New permission `alert:route_manage` (platform scope only) for channel/route writes, separate from
  `alert:manage` (ack/resolve). Coordinate with K1 (owner of `auth/permission.go`).
- **Tenant-scoped routing is NOT built.** Extension path when a tenant-owned Kind exists: `scope='tenant'`
  routes with `tenant_id NOT NULL` under a new `alert_routes_tenant_owned` family keyed on `app.tenant_id`,
  routing only that tenant's tenant-owned alerts. Whether a B2B operator may receive notifications about
  **platform-owned** alerts where it is the subject is a human/product decision (§12 H7); until then never.

## 9. Configuration (names only; values are technical defaults unless noted)
| Env | Default | Notes |
|---|---|---|
| `ALERT_DISPATCH_INTERVAL_SECONDS` | 15 | Replaces the `DefaultLoopInterval` constant in `main.go:497` |
| `ALERT_CLAIM_LEASE_SECONDS` | 120 | |
| `ALERT_DELIVERY_P1_MAX_ATTEMPTS` / `_P2_` / `_P3_` | 8 / 5 / 3 | |
| `ALERT_DELIVERY_P1_BACKOFF_CAP_SECONDS` / `_P2_` / `_P3_` | 300 / 60 / 60 | |
| `ALERT_ROUTING_READINESS_MODE` | `report` (non-prod); **required, no default, in production** | `enforce` \| `report` |
| `ALERT_MOCK_CHANNEL` | `disabled` | `enabled` refused when `GuardEnvironment()=="production"` |

DB-held configuration (never env): routes, channels, `escalate_after`, `recipient_ref`,
`credential_secret_ref` (e.g. shape `awssm://arn:aws:secretsmanager:<region>:<acct>:secret:<prefix>/platform-alerting/<channel_key>?versionId=<id>`;
**no value, ARN or name is chosen or seeded by this design**). Secret backends allowed per environment
follow `config.ValidateSecretBackendScheme` (awssm only in production).

## 10. HTTP surface (platform admin only, audited)
| Endpoint | Permission | Behaviour |
|---|---|---|
| `POST /v1/admin/alerting/channels` | `alert:route_manage` | Body: `channel_key, channel_kind, credential_secret_ref?, enabled, reason_code, supersedes_id?`. Server: `ParseRef` + platform-alerting namespace; fetch secret **outside** tx and fingerprint (refuse on any fetch failure: fail-closed); refuse `mock` kind in production; refuse kind with no `Channel` wired. Tx: supersede + insert + audit + raise `alerting.route_changed` |
| `POST /v1/admin/alerting/routes` | `alert:route_manage` | Body: `severity, kind?, escalation_step, channel_key, recipient_ref?, escalate_after?, enabled, reason_code, supersedes_id?`. Go: `Channel.ValidateRecipient`; DB: R1-R5. Same tx shape |
| `GET /v1/admin/alerting/channels`, `GET /v1/admin/alerting/routes` | `alert:route_manage` | Current + history; secret ref shown (it is a handle, staff-visible like provider creds), fingerprint shown, never a value |
| `GET /v1/admin/alerting/status` | `alert:manage` | Readiness per severity, counts by `delivery_state` and severity, last pass time |
| `GET /v1/admin/alerts?state=&severity=&delivery_state=&cursor=` | `alert:manage` | Paged list from the view (operators need alert ids to ack) |
| `POST /v1/admin/alerts/{id}/ack` (existing) | `alert:manage` | Adds optional `reason_code` |
| `POST /v1/admin/alerts/{id}/resolve` (existing) | `alert:manage` | Unchanged |

Audit (every mutating call, success **and** refusal 403/404/409/422): `audit.Entry` with actor, `TenantID=Nil`,
IP, UA, request id, `Action` (`alerting.channel_version_created`, `alerting.route_version_created`,
`alerts.ack`, `alerts.resolve`, plus `.denied` outcomes), target, metadata `{before: <prior version id +
enabled + channel_kind + severity/kind/step>, after: {...}, reason_code}`; **recipient_ref and secret ref are
recorded by fingerprint in audit metadata, not verbatim** (audit is wider-read than the config tables).
Subject-bearing alert actions keep `SubjectTenantID` (ADR 0104). The `alerting.route_changed` raise uses
`alerting.InTx` with the platform-admin runner; the route write must not depend on the raise (savepoint
rule, ADR 0102 §7.2).

## 11. Metrics (no tenant label anywhere)
New: `alert_routing_ready{severity}` gauge; `alert_delivery_attempts_total{channel_kind,result=sent|failed|dead,error_class}`;
`alert_channel_send_duration_seconds{channel_kind}` histogram; `alert_escalations_total{severity,cause=unacked|dead}`;
`alert_open{severity,delivery_state}` gauge (computed per pass); `alert_route_changes_total{object=route|channel}`;
`alert_channel_credential_fetch_failures_total{class}`. Changed: `alert_unrouted_total{severity,reason}`.
Kept: raise failures, dead, stale claims, dispatcher passes.

## 12. Runbook content (`docs/runbooks/observability-and-alerting.md` §3 updates)
1. Replace "How routes will be configured" with the endpoint flow: create channel (disabled) → create route
   (disabled, recipient optional) → enable channel → enable route; each step's audit and the
   `alerting.route_changed` alert; what R1-R5 refusals mean.
2. "Activation checklist" listing the §13 operator inputs; explicit sentence: **a `log` or `mock` route is not a
   notification to any person; `alert_routing_ready` stays 0 until a `human_notification` channel exists.**
3. Runbooks for `alert_routing_ready == 0`, `unrouted` by reason (`no_route`, `channel_disabled`, `no_sink`,
   `channel_not_eligible`), `delivery_failed` (dead) and `alert_channel_credential_fetch_failures_total`.
4. Credential rotation: new secret version → new channel version (supersede), routes untouched.
5. Out-of-band: the external monitor must alert on `alert_routing_ready==0`, `alert_dead_total` increase,
   and a flat `alert_dispatcher_passes_total`, through a path that does not depend on this dispatcher.

## 13. Test matrix (T-1 injectable clock/rand; no wall-clock assertions; local runs never labelled CI)

### Unit
- `ErrorClass.Retryable` table (5 classes + empty). Backoff/jitter per severity with seeded rand; cap; shift clamp.
- Due-work classification table incl. new `dead`→p1 escalate, p2/p3 terminal.
- Readiness evaluator table: no route; route disabled; channel disabled; kind not human_notification (`log`);
  synthetic in production; kind not wired; kind-specific route only; all good (with a test-only fake
  human_notification kind inserted by a test migration fixture, never by 0117).
- Config: `ALERT_MOCK_CHANNEL=enabled` + production refused; readiness mode unset in production refused;
  invalid values refused.
- Fetch class → ErrorClass mapping table.

### Integration (real Postgres, `-race`)
- Activation: route insert with `enabled=true` and missing channel / missing recipient / disabled channel /
  credential-requiring kind without ref → refused (R1/R2); disabled route with NULL recipient accepted.
- R3 uniqueness; supersede-and-insert in one tx works (deferred FK); double current version refused.
- R5: superseding/disabling the last enabled human-notification p1 step-0 route refused; allowed when a
  replacement is enabled in the same tx; not triggered before any such route ever existed.
- Channel secret-ref CHECK: `/provider-creds/` path refused; missing `/platform-alerting/` refused; control
  chars refused; ref without fingerprint refused.
- Delivery: route via `RecordingChannel` → `sent` with `receipt_fp`; scripted retryable failures → backoff
  → sent; permanent failure → dead after 1 attempt; budget exhaustion → dead + `alerting.delivery_dead` +
  metric; p1 dead with step-1 route → step-1 sent; p2 dead terminal; unacked escalation; acked → no
  escalation; resolved → nothing further; hung channel → timeout class, no stranded claim; panic in Send →
  failed/unknown and the next alert in the same pass is still delivered.
- N-4: route to a kind with no wired channel → `unrouted/no_sink` row + meta + metric (today: log only).
- `channel_not_eligible`: synthetic route with production guard env → unrouted, `Send` never called.
- Kind-specific route wins over severity default; falls back when kind route disabled.
- Two dispatchers → one claim, one notification per DedupKey (RecordingChannel duplicate counter == 0 human
  duplicates).
- Credential: fetch failure (memstore not_found) → misconfigured → dead; store_unavailable → retryable;
  `Send` receives the secret, captured logs never contain it.
- Post-commit (LF): a financial path (deposit receipt T10 multiple-success fixture) commits and its ledger
  rows exist while the channel is scripted to fail/panic/hang; ledger SUM(debits)=SUM(credits) unchanged.
- View `alert_delivery_status` returns each `delivery_state`.

### RLS / tenant isolation
- Tenant, tenant-principal, player, acting-GUC and mixed-GUC sessions: 0 rows from `alert_channels`,
  `alert_routes`, `alert_channel_kinds` writes refused, view rows limited to existing 0110 visibility
  (subject tenant sees its subject alerts, no delivery columns beyond what the view derives — **view must
  not expose route/channel/recipient columns**).
- Dispatcher: SELECT channels/routes OK; INSERT/UPDATE channels/routes refused; meta-only alert INSERT
  still exactly three Kinds (`alerting.route_changed` from dispatcher refused).
- Platform admin with unvalidated principal refused (`alerting_validated_platform_admin`).

### Authorization (HTTP, each layer pinned separately like S-6)
- Tenant staff with any role → 403 on all new endpoints; platform admin without `alert:route_manage` → 403
  on writes; with it → 200; player token → 401/403; permission layer removed (test seam) → explicit
  platform-scope check still 403; both removed → DB refuses.

### Audit
- Every successful write: exactly one audit row, actor/IP/UA/request id, before/after, reason code, secret
  ref + recipient by fingerprint only. Every refusal (403/404/409/422): one denied audit row, no state change.
- Ack with reason_code recorded; resolve unchanged; subject tenant sees subject alert ack audit read-only.
- `alerting.route_changed` raised once per write; route write commits even if the raise is forced to fail
  (P0001 injection via `alertinject`).

### Static / wiring
- `alertingtest` imported only by tests and the dev-wiring file.
- `main.go`: channels registered in `buildRegistrations`; `RecordingChannel` only under the dev gate;
  production guard refuses it (extend `alert_dispatcher_wiring_test.go` with negative controls).
- `db.ServiceAlertDispatcher` reference set unchanged.
- No migration or non-test Go file inserts into `alert_routes`/`alert_channels` (grep-style static test over
  `migrations/` and non-test `.go`).
- 0117 up then down on an empty scratch DB; `migrate verify`.

### Mutation targets (must be killed)
R1 CHECK removed; R2 trigger channel-enabled check removed; R5 last-route guard removed; namespace CHECK
`/provider-creds/` clause removed; `enabled` default flipped to true; `Retryable()` returns true for
`misconfigured`; escalate-on-dead applied to p2; `no_sink` branch reverted to log-only; synthetic-in-prod
check removed; readiness counts `log` as human; readiness counts kind-specific routes; `/readyz` enforce
ignores readiness; config allows mock channel in production; dispatcher fetches secret inside the read tx;
`Send` panic not recovered per alert; denied-audit write removed; audit records recipient verbatim;
tenant-scope check removed from a new handler; view loses `security_invoker`.

## 14. Explicitly NOT done by this workstream
- No real channel adapter (pager/chat/email/SMS/webhook): **NOT IMPLEMENTED, PROVIDER DEPENDENT**. No
  `human_notification=true` kind exists in the vocabulary after 0117.
- No recipient, rota, contact or secret is created, seeded or invented anywhere (fixtures use the `mock`
  kind and opaque test refs inside tests only).
- ALERT-RETENTION-1 (unbounded occurrences/deliveries) not addressed.
- K2 "a new unlinked-adjustment breach must page" (per-tx discriminator,
  `internal/reconciliation/alerts.go:41,80`) not addressed: owned by reconciliation + ledger-finance,
  independent of 0117.
- Inbound vendor ack (acknowledge from the pager) not built; it is part of the real adapter.
- Tenant-scoped routing and tenant-visible delivery state not built (§8).
- Partner-console/back-office UI not built (API only).

**ALERT-DELIVERY-1 stays OPEN** after this workstream merges. Closure still needs registry conditions
(a) human-authored routes, (b) a real non-log non-MOCK channel, (c) a named human recipient/on-call,
(d) the §17.7 preconditions; this workstream delivers the SR-7 route authoring, N-4, mock-refusal,
credential-by-reference and conformance-suite parts of (d), nothing of (a)-(c).

## 15. Operator / human inputs still required (none decided here)
| # | Input | Owner |
|---|---|---|
| H1 | Channel provider choice (paging vendor, chat, email, SMS, or several) and the commercial contract; also vendor data-location for alert payloads (ids only, but leaves the platform) | human (commercial + legal) |
| H2 | Recipients: which on-call service/rota/escalation policy per severity and step, expressed as the vendor's opaque target refs | human / operations (HD-PRH2-4-OPS) |
| H3 | On-call policy: coverage hours, who is primary/secondary, ack expectations per severity | human / operations |
| H4 | Escalation windows (`escalate_after` per severity/step) and whether p2 escalates at all | human / operations |
| H5 | `ALERT_ROUTING_READINESS_MODE` in production (`enforce` blocks traffic until p1/p2 routing is human-notifying; `report` does not) | human (launch decision; RECOMMENDATION: `enforce`) |
| H6 | The secret: who provisions the vendor credential in the production secret store under the `platform-alerting` namespace, its ARN/version, rotation owner | human / devops |
| H7 | Whether B2B operators may ever receive notifications about platform-owned alerts naming them | human / product + legal |
| H8 | Out-of-band notification target for the external monitor (dead dispatcher / dead channel) — itself a recipient | human / operations |
| H9 | Whether ALERT-DELIVERY-1 blocks launch of money-moving flows (already flagged, ADR 0102 §17.7 item 8) | human |
| T1 | Retry budgets and intervals (§9) | devops review (technical, not human) |
| S1 | Dev-profile mock wiring (§6) and the `alerting.route_changed`-instead-of-four-eyes choice for SR-7(i) | security review |

## 16. Implementation order (one engineer, one branch)
1. 0117 up/down + migration tests (RLS, CHECKs, R1-R5, view, route_changed Kind).
2. `Channel` contract, `LogChannel`, `alertingtest.RecordingChannel` + conformance suite; migrate tests off `MockSink`.
3. Dispatcher: resolution with reasons, permanent/retryable, budgets, per-alert panic recovery, credential
   fetch, p1 escalate-on-dead, readiness evaluator + metrics.
4. HTTP: channel/route writes, reads, status, alert list, ack reason, denied audit; permission via K1.
5. Wiring: config vars, `buildRegistrations`, readiness mode + `/readyz`, dev gate; wiring tests.
6. ADR 0102 §18, runbook §12, registry row update (ALERT-DELIVERY-1 remains OPEN), mutation evidence file.
