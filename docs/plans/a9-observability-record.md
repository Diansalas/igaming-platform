# A9 record: observability + alert delivery plumbing (2026-10-15)

Branch `a9-observability` (from `f9a31e7`, "Apply A2 proposed registry rows"). NOT pushed, NOT merged. This file is the
proposed registry text for the orchestrator to apply; `docs/governance/task-registry.md`, `docs/HANDOVER.md`,
`docs/progress.md` and `docs/active-stage.md` were deliberately NOT edited.

Scope input: audit item A9 (`docs/plans/platform-completion-audit-2026-10-14.md`) and the owner decision of 2026-10-15
(alert delivery = email + ONE webhook, owner as interim recipient, configured only via env/config).

## 1. Labels (CLAUDE.md "no fake completion")

| Deliverable | Label | Notes |
|---|---|---|
| Prometheus `/metrics` on a separate listener (`METRICS_ADDR`) | IMPLEMENTED | OTel Prometheus exporter v0.68.0 (client_golang v1.24.1), private registry, separate `http.Server`, off unless set, never on the API mux (static pin), production refuses a non-loopback bind without `METRICS_ALLOW_NON_LOOPBACK=true` |
| Pending signals emitted: DB pool saturation, reconciliation staleness (per stream, oldest tenant), reconciliation mismatches gauge, sweep heartbeat, alert dispatcher backlog/age | IMPLEMENTED | in-process state; per-instance series; no tenant label |
| Payment kill-switch STATE gauge | NOT IMPLEMENTED | needs a cross-tenant read identity for a FORCE-RLS table; engage events already raise the durable alert `payment.kill_switch_engaged`. Still listed in `pending-signals.yml` with the reason |
| Provider-call latency SLO metric, per-tenant request SLO | NOT IMPLEMENTED | listed in `pending-signals.yml` (PROVIDER DEPENDENT / needs a bounded tenant-label policy decision) |
| Alert rules wired to emitted series (12 new rules) | IMPLEMENTED (rules + parity tests + scrape/compose example) | Prometheus/Alertmanager NOT DEPLOYED anywhere; no receiver; headers say `EMITTED AND SCRAPEABLE; NOT DEPLOYED` |
| OTLP trace/metric export to a backend | NOT IMPLEMENTED | no backend exists (PROVIDER DEPENDENT); traces still go to the stdout exporter |
| Webhook alert channel (HMAC-SHA256, timestamp, idempotency key, SSRF guard, no redirects, bounded timeouts, secret by reference) | IMPLEMENTED | `internal/alerting/channels`, `internal/egressguard`; delivers nothing until a route is live and a receiver exists |
| Email alert channel over `internal/email` | PARTIALLY IMPLEMENTED | sink IMPLEMENTED; the only provider is `email.MockProvider` (MOCK), so it reaches NO person; production refuses MOCK. A real SMTP/API adapter is PROVIDER DEPENDENT |
| SR-7 four-eyes route approval (migration 0156) | IMPLEMENTED | DB-enforced: stage disabled by admin A, enable by a DIFFERENT admin B within 7 days, once per staging |
| `reconciliation.sweep_failed` alert (R3-LISTING-FAILURE-LOG-ONLY-1) | IMPLEMENTED | new Kind, p1, platform, subjectless; listing failures and sweep panics; raised under the dispatcher identity |
| `reconciliation.run_failed` (RECON-RUN-FAILED-ALERT-1) | IMPLEMENTED (already; verified and pinned) | raised from all five stream functions since I-wire; the registry row text was stale; A9 adds a static pin |
| Alert history retention (ALERT-RETENTION-1, migration 0157) | PARTIALLY IMPLEMENTED | mechanism IMPLEMENTED; **off by default**: the retention horizon is a legal/compliance decision (HUMAN). Open alerts' occurrences are retained until resolution |
| Runbooks | IMPLEMENTED | `docs/runbooks/alert-delivery-channels.md` (new), `observability-and-alerting.md` s4, `incident-response-and-oncall.md`, `production-configuration-checklist.md`, `README.md`, `.env.example` |
| **ALERT-DELIVERY-1** | **STAYS OPEN** | see s3 |

## 2. Proposed registry text

**A9 (audit item) -> PARTIALLY IMPLEMENTED (2026-10-15, branch `a9-observability`, not merged).** Metrics exposition, pending
signals, wired rules, email + webhook channels, SR-7 four-eyes, `reconciliation.sweep_failed`, retention purge and runbooks are
implemented and tested (see this record). NOT done: kill-switch state gauge, provider-latency and per-tenant SLO metrics, OTLP
export, a real email provider, a webhook receiver, a deployed Prometheus/Alertmanager and on-call rota.

**RECON-RUN-FAILED-ALERT-1 -> CLOSED (verified, no new code needed for the per-tenant case).** `reconciliation.run_failed`
(p1, detached, discriminator `stream:<stream>[:provider:<id>]`) has been raised from all five stream functions since I-wire
(`internal/reconciliation/alerts.go raiseRunFailed`; the `scheduler.go:314,407,...` line numbers in the old row no longer apply).
A9 adds `TestStatic_EveryStreamRaisesRunFailed` so it cannot silently regress. The remaining log-only sweep-level failures are
covered by `reconciliation.sweep_failed` (next row).

**R3-LISTING-FAILURE-LOG-ONLY-1 -> CLOSED (engineering).** A tenant listing failure (active or non-active) or a sweep panic in
`RunSchedulerLoop` raises the durable alert `reconciliation.sweep_failed` (p1, platform scope, no subject, attributes `phase` in
{list_active, list_non_active, panic, unknown} and `sqlstate_class`, discriminator `phase:<phase>`, dedup per phase) via
`alerting.RaiseSweepFailed` under the alert_dispatcher identity (migration 0156 widens exactly its INSERT policies by this Kind,
no privilege change for the scheduler). Graceful-shutdown cancellation raises nothing. The panic log now carries the panic TYPE
only. Runbook: `observability-and-alerting.md` s4. Delivery of that alert to a person is ALERT-DELIVERY-1.

**ALERT-RETENTION-1 -> PARTIALLY IMPLEMENTED; no longer a code blocker, now a HUMAN decision.** Chosen design (not
partitioning): `alerting_purge_resolved(window, batch)` (migration 0157, SECURITY DEFINER, dispatcher-identity sessions only):
removes resolved alerts older than the window (floor 7 days, enforced in the DB and in config) with their occurrences and
deliveries; open/acked alerts never; runtime role keeps no DELETE; DELETE triggers split from the 0110 guards and refuse
anything outside the function (flag + table owner + row eligibility); the only RLS additions are dispatcher-identity + flag DELETE
policies (no SELECT policy, no role-dependent expression: the KYC worker catalogue gate requires role-independent policies); every run audited in append-only `alert_retention_runs`;
`ALERT_RETENTION_DAYS` unset = no job (startup warning). OPEN: the retention horizon per jurisdiction (legal), purge of the
occurrences of a long-lived OPEN alert, archival of purged rows (none: deleted rows are gone, restore only from backup).

**ALERT-DELIVERY-1 -> STAYS OPEN.** Done in A9: provider-neutral email and webhook channels behind the ChannelKind seam;
SR-7 four-eyes built; webhook passes the channel conformance suite; recipients are platform config. Still required to close it
(a-d of ADR 0102 s17.7 as already recorded): (a) a human-authored route staged by one admin and enabled by a SECOND platform
admin; (b) over a channel that is neither log nor mock: a REAL email provider adapter (PROVIDER DEPENDENT) and/or a webhook
receiver URL + signing secret provisioned in the secret store (owner input); (c) to a named recipient (interim: the owner,
`docs/runbooks/alert-delivery-channels.md` s1); (d) security re-review of the new channel code and the SR-7 migration (this
record lists the probes), and the remaining s17.7 items (a new K2 breach pages).

New rows proposed:

| ID | Owner | Status | Text |
|---|---|---|---|
| ALERT-EMAIL-PROVIDER-1 | backend + owner | OPEN, PROVIDER DEPENDENT | Real email delivery adapter implementing `email.Provider` and marked production-eligible (`providerkind.ProductionEligible`). Until then the email alert channel reaches no person and production refuses the MOCK provider. |
| ALERT-WEBHOOK-ENDPOINT-1 | owner | OPEN, HUMAN INPUT | Choose the webhook receiver (URL), provision its signing secret (>= 32 bytes) in the secret store, set `ALERT_WEBHOOK_URL` and `ALERT_WEBHOOK_SECRET_REF`. |
| ALERT-SECOND-ADMIN-1 | owner | OPEN, HUMAN | SR-7 needs TWO distinct platform admins (stage and enable). A single-owner deployment has one; a second platform admin must be created through the staff lifecycle before any human route can be enabled. |
| ALERT-RETENTION-HORIZON-1 | owner + compliance | OPEN, HUMAN/LEGAL | Decide `ALERT_RETENTION_DAYS` per jurisdiction before production; until set, alert history is unbounded. |
| OBS-BACKEND-DEPLOY-1 | devops | OPEN | Deploy Prometheus + Alertmanager (or a managed equivalent) scraping `METRICS_ADDR` with a network path restricted to the collector; route `severity=page` to a human; pin the compose image by digest. `deploy/observability/{prometheus.yml,docker-compose.observability.yml}` are examples only. |
| OBS-OTLP-TRACES-1 | devops | OPEN, PROVIDER DEPENDENT | OTLP trace/metric export once a backend exists (traces/metrics periodic dump remain stdout). |
| OBS-PENDING-SIGNALS-2 | domain specialists | OPEN | Remaining pending signals: payment kill-switch engaged state (needs a cross-tenant read identity), provider-call latency (needs real providers), per-tenant SLO (needs a bounded tenant-label policy). See `deploy/observability/pending-signals.yml`. |
| ALERT-DELIVERY-STATE-DISPLAY-1 | backend | LOW | `alerting.DeliveryState` / route DTO `human_notification` follow the channel KIND, so an email route over the MOCK provider displays `delivered`/`human_notification:true` in the admin list; `GET /v1/admin/alerting/status` (readiness) is accurate. Dev-only; production refuses MOCK. |

## 3. Proposed ADR 0102 amendment text (section 19, "A9 delivery channels, SR-7 four-eyes, sweep failure, retention")

1. **Channels.** `ChannelKind` gains `email` and `webhook` (migration 0156 widens both `channel_kind` CHECKs). Both stay
   fail-closed human-notification kinds (`alerting_channel_kind_is_human_notification` is unchanged: only log and mock are
   non-human). A sink's own `HumanNotification()` decides readiness: an email sink over a synthetic provider and a
   development-posture webhook sink answer false. Sinks live in `internal/alerting/channels` (not `alerting`, to keep the
   reconciliation package's reviewed transitive dependency set free of HTTP and email code).
2. **Recipients and secrets.** Email recipients and the webhook URL are platform configuration (`ALERT_EMAIL_RECIPIENTS`,
   `ALERT_WEBHOOK_URL`); `recipient_ref` stays an opaque label and is never an address or URL. The webhook signing secret is a
   secret-store reference (`ALERT_WEBHOOK_SECRET_REF`).
3. **SR-7.** Enabling a route for a human-notification kind requires `staged_route_id`: a current, DISABLED, identical route
   created by a different validated platform admin at most 7 days earlier, consumable once. Audit records staged route id and
   stager. Replaces "four-eyes not built".
4. **Kind `reconciliation.sweep_failed`** (p1, platform, subjectless, detached): see R3-LISTING-FAILURE-LOG-ONLY-1.
5. **Retention.** `alerting_purge_resolved` (not partitioning): see ALERT-RETENTION-1.
6. **Payload.** Channels carry kind, severity, alert id, dedup key, discriminator and subject tenant id only; never attribute
   values (some Kinds record operator-entered text).

## 4. Migrations

`0156_alert_channels_and_sweep_failed_kind` and `0157_alert_retention` (up AND down; FORCE RLS unchanged on every table; every
new function pins `search_path = pg_catalog, public, pg_temp`). The gap 152-155 (and the older 143-150) is intentional on this
branch: other branches own those numbers; `cmd/migrate verify` reports the gaps and nothing else (no checksum mismatch).
Downs refuse (AR099) when data would be lost: email/webhook routes or staging records, `reconciliation.sweep_failed` alerts, and any
`alert_retention_runs` row.

## 5. Behaviour and structure notes for reviewers

- `cmd/platform-api`: `buildAlertSinks` (observability_wiring.go) builds channels only when configured; `alertWebhookSecretFunc`
  lives in registrations.go (the S-4 pin allows `secretstore` construction only there); `Dispatcher.AddSinks` registers them before
  the first pass and then closes the sink map (immutable while passes read it).
- `RunSchedulerLoop` still calls `RunSweep` directly (ADR 0113 D pin); the new `handleSweepError` / `recoverSweepPanic` wrap it.
- `internal/observability.metricsOnly` is a plain handler (no ServeMux) so the repo-wide "no route registration outside
  internal/httpserver" pin still holds.
- Reconciliation state is per process and starts empty after a restart; the first sweep runs immediately.

## 6. Changed assertions (none weakened without replacement)

| File / test | Change | Why |
|---|---|---|
| `internal/alerting/alert_test.go` `TestKinds_ScopeSubjectConsistency` | `KindReconciliationSweepFailed` added to the subjectless allow-list (still asserts RequiresSubject=false and InTxRaisableByTenant=false for it) | the new Kind mirrors migration 0156's widened `alert_kinds_check1` |
| `internal/alerting/loop_readiness_test.go` `TestDeliveryState_UnknownKindsNeverReadAsDelivered` | `"webhook"` replaced by `"sms"` in the unknown list; new positive assertion that `email` and `webhook` read as delivered | webhook is now a known human kind |
| `internal/alerting/migration_0117_integration_test.go` `...NoRouteCanBeEnabledForAHumanNotificationKind` | message substring `"...required; not built"` relaxed to `"...required"` (AR001 code still asserted) | "not built" is no longer true; the staged-approval tests (`migration_0156_integration_test.go`) assert the new messages |
| same file `expectedPinnedAlertFunctions` | + `alert_retention_delete_guard`, `alert_retention_runs_guard`, `alerting_purge_resolved` (each still must pin search_path) | migration 0157 functions |
| `internal/alerting/alertingtest/conformance.go` | `Subject.DeclaresHuman`, `Subject.FailClasses`; the not-human case now requires an EXPLICIT declaration for a human channel (and fails a channel that declares human but reports false) | real human channels exist now; stricter in one direction, necessary generalisation in the other |
| `deploy/observability/rules_parity_test.go` `TestRuleFilesAreHonestAboutBeingUnwired` -> `...AboutTheirStatus` | the metric rules file must say `EMITTED AND SCRAPEABLE`, `NOT DEPLOYED`, `ALERT-DELIVERY-1 is OPEN` and must NOT say `STATUS: NOT WIRED`; log-event rules and pending signals keep the old NOT WIRED requirement | the metric series are now emitted and scrapeable |
| `TestPendingSignalsAreStillAbsent` fixture (`pending-signals.yml`) | three entries promoted to real rules; three genuinely-missing entries listed | the guard worked as designed: it failed until the entries were promoted |

## 7. Security review: what to probe

1. **Webhook SSRF** (`internal/egressguard`, `internal/alerting/channels/webhook.go`): URL validation table; dial-time address check
   (DNS rebinding, v4-mapped, NAT64/6to4/Teredo, CGNAT, metadata 169.254.169.254); redirects; env proxy; header/body size;
   whether the URL could ever come from an alert, a route row or `recipient_ref` (it must not); log lines (no URL, no secret).
2. **Webhook signing**: HMAC over `timestamp + "." + body`; receiver verification helper; replay window is the receiver's job
   (documented); secret length floor, 5-minute cache, rotation by version-pinned reference; secret never in headers, body, logs.
3. **SR-7 migration 0156**: can one admin create both halves (two sessions, same id)? staged-row race (unique index);
   `staged_route_id` on non-human rows; the guard function reading `alert_routes` under RLS; the dispatcher-identity policy widening
   (only `reconciliation.sweep_failed`; tenant and platform-admin paths for it).
4. **Retention 0157**: `alerting_purge_resolved` SECURITY DEFINER surface (identity is GUC-asserted like every alert RLS family: a
   compromised app process can invoke it but only for resolved alerts older than the 7-day floor, audited); the flag + owner +
   eligibility triple in `alert_retention_delete_guard`; the GUC-only DELETE policies (forgeable by design, harmless without the
   DELETE privilege that only the owner has); runtime privileges; the downgrade refusals.
5. **Metrics exposure**: separate listener, no auth, bind validation (`config/observability.go`), what the exposition reveals
   (Go/process collectors), cardinality (no tenant label), `MaxRequestsInFlight`, the static "never on the API mux" pin.
6. **Email**: header/line injection through the discriminator and recipients; MOCK refusal in production; body content (ids only).
7. **Config**: `ALERT_*` and `METRICS_*` validation, startup failure semantics, nothing secret printed.

## 8. Mutation pass

One mutant at a time, relevant suite re-run, source restored after each (runner kept outside the repo). Totals: 84 unit mutants
(egressguard, webhook, email, alerting core, reconciliation tracker/sweep alert, config, metrics handler, cmd wiring) + 28 SQL
mutants (migrations 0156/0157 through the real database tests) + 4 policy-redesign mutants.

- Survivors that revealed test gaps were fixed by strengthening tests, then re-killed: v4-mapped CGNAT/TEST-NET ranges and the
  `.localhost` sub-domain under the development allowance (egressguard); the refused-destination error class and the
  future-timestamp replay window (webhook); the tick result for error vs panic (retention loop); lock-skip must create NO tracker
  state, asserted by absence rather than by a zero default (reconciliation); retention-window floor tested with a valid
  interval so the window is the only possible refusal reason, RFC 5322 comment recipients, required-vs-malformed secret
  reference (config); the 7-day staging expiry, other channel kinds (`sms`, `slack`) refused in BOTH channel CHECKs, the
  actionable no-staging message, the 7-day floor of the alerts-row delete guard, and the audit table's own append-only trigger
  with RLS lifted (migrations).
- Invalid mutants (did not compile) were rewritten and re-run.
- **Classified equivalent / defense-in-depth, not observable by any available test**: email `M5` and `M10` (the top-level
  dedup key is redundant with the per-recipient dedup); webhook `W3` (constant-time compare is not behaviourally observable;
  not executed); SQL `S29` (the explicit REVOKE restates the 0110 grants); `S31` (DELETE policy without the dispatcher
  identity: the existing dispatcher SELECT policies already hide the rows); `S32` (DELETE policy without the flag: the trigger
  refuses instead, no data changes); `S33`/`S34` (trigger owner check / flag check: each is shadowed by another layer, and
  exposing them would need a non-owner role holding DELETE, which a test may not create).
