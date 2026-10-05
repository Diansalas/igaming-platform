# Scope verdict: ALERT-DELIVERY-1 continuation design (0117)

Reviewer: product-owner-proxy. Date: 2026-10-05. Read-only review; no repo edits.

## Verdict
Design is correct in direction but about twice the size the owner's list needs. The
reason is one decision: it builds the full real-channel substrate (versioned channels, secret
refs, receipts, last-route guard, enforce mode) with no real channel, vendor, credential or
recipient in existence. That is speculative, shaped by an unknown vendor, and every deferred
piece is additive later. Cut to routes + fail-closed activation + severity/retry + readiness
signal + audit + test channel. Security, audit, tenant isolation and fail-closed items are
never cut on MVP grounds; none of the cuts below touch them.

## Classification

| Element | Class | Why |
|---|---|---|
| routes.enabled DEFAULT false | MUST | fail-closed activation |
| R1 CHECK (enabled needs channel + recipient) | MUST | fail-closed |
| recipient_ref DROP NOT NULL | MUST | else operator must invent a placeholder recipient (fake recipient) |
| routes.reason_code (trigger-required) | MUST | audit reason code |
| deliveries.unrouted_reason (no_route, channel_disabled, no_sink) | MUST | N-4: black-holed route must be a visible row, not a log line |
| `channel_not_eligible` reason | DEFER | mock is never wired in a binary, so prod mock route already yields `no_sink` |
| routes.kind override column | DEFER | nothing needs per-Kind routing; severity+step is enough |
| routes.channel_key + alert_channels table (versioned) | DEFER | exists to hold credentials; no real kind, so nothing to hold |
| credential_secret_ref, fingerprint, namespace CHECK, Fetcher integration, fetch-class mapping, fetch metric | DEFER | no credential exists; design against the real vendor |
| alert_channel_kinds table (+ FK swap of CHECKs) | DEFER | use a Go-side `HumanNotification()` flag on the channel (log=false, mock=false) with a pinning test; real-kind migration widens the existing CHECKs |
| human_notification semantics (log/mock never count) | MUST | owner: log is not notification |
| R2 channel-enabled trigger | DEFER | no channels table |
| R3 unique current route | SHOULD-NOW only if resolveRoute ambiguity is observed; otherwise DEFER | "latest effective_from" already deterministic |
| R5 last-p1-route guard | DEFER | cannot trigger until a human route exists; keep as a named closure precondition (SR-7 ii) |
| New Kind `alerting.route_changed` + CHECK rewrite + temp policy | DEFER | audit row already records every route write; nobody to page |
| Retryable/permanent split, permanent -> dead immediately | MUST | retry/failure behavior |
| Severity budgets p1/p2/p3 (constants in config struct, tested) | MUST | severity support |
| 7 per-severity env vars + interval/lease env | DEFER | defaults in code; envs add prod-config surface |
| Backoff jitter | DEFER | single dispatcher, no herd |
| p1 escalate-on-dead | SHOULD-NOW | small classification branch; this is what makes p1 distinct |
| Per-alert panic recovery in processOne | SHOULD-NOW | real starvation bug, few lines |
| Sink -> Channel rename | DEFER | pure churn; extend in place |
| ValidateRecipient hook | SHOULD-NOW | cheap, enforces no-email/phone shape per channel |
| Credential param in Send | DEFER | zero-value today; adding it with the first adapter is mechanical |
| ReceiptRef / receipt_fp | DEFER | add with the adapter |
| Test channel: mutex, scripted outcomes, hang-until-ctx, panic, dedupe counter | MUST | owner asked for a test/mock channel |
| Conformance suite (dedupe on DedupKey, ctx, classification, no secret/body in logs) | SHOULD-NOW, slim | closure precondition (d) needs it; tests only; run on Recording + Log |
| Conformance markup-rendering case | DEFER | attributes already charset-restricted |
| Dev wiring gate `ALERT_MOCK_CHANNEL` | DEFER | changes a security pin, needs security sign-off, tests-only channel satisfies the owner. Loss: no manual dev end-to-end demo |
| N-5 register LogChannel with providerkind guard | DEFER (stays on follow-up list) | |
| Route authoring `POST/GET /v1/admin/alerting/routes` | MUST | config is raw SQL and unaudited today; CLAUDE.md requires audit on mutating admin actions |
| Channel authoring endpoints | DEFER | no channels table |
| Permission `alert:route_manage` | MUST | acker must not be able to redirect pages (separation); coordinate K1 |
| Platform-only scope + explicit tenant check + RLS | MUST | tenant isolation |
| Tenant-scoped routing | DEFER (as designed) | write the extension path only |
| Audit on success and refusal (403/404/409/422), recipient by fingerprint | MUST | auditability |
| Refused ack/resolve audit | MUST | known gap |
| Ack reason_code | SHOULD-NOW | open follow-up, trivial |
| GET /alerting/status (readiness, counts, last pass) | MUST | visible not-ready |
| GET /admin/alerts list | SHOULD-NOW | operators cannot ack without ids |
| View alert_delivery_status (security_invoker, no route/recipient columns) | SHOULD-NOW | derived delivery_failed; agree with not adding a stored alerts.state (AL-10) |
| Readiness evaluator + `alert_routing_ready{severity}` + Error log on transition | MUST | owner: no silent unready |
| `/readyz` enforce mode + prod-required env | DEFER | blocking traffic is a launch decision (H5/H9); report-only now. Loss: no automatic hard gate |
| Metrics: ready gauge, unrouted{severity,reason}, attempts_total{channel_kind,result,error_class} | MUST | |
| Metrics: send-duration histogram, escalations, alert_open gauge, route_changes, credential failures | DEFER | |
| Meta-alert delivery_dead | already in scope | unchanged |
| Post-commit LF test (one case) | SHOULD-NOW | proves delivery cannot affect ledger |
| Runbook: activation checklist, exact operator inputs, "log/mock is not a notification" | MUST | owner |
| Down migration (empty scratch DB) | MUST | for the reduced 0117 |
| Mutation list | trim to surviving items | R1, default-false, Retryable, no_sink revert, readiness-counts-log, tenant check, denied-audit, panic recovery, view invoker |

## Smallest cut
0117: ALTER alert_routes (enabled default false, reason_code, recipient_ref nullable, R1 CHECK);
ALTER alert_deliveries (unrouted_reason); view alert_delivery_status. Go: retryable/permanent,
severity budgets, p1 escalate-on-dead, per-alert panic recovery, no_sink row, readiness evaluator,
Go-side human_notification flag, test channel + slim conformance. HTTP: route POST/GET, status,
alert list, ack reason, denied-audit. Docs: ADR 0102 §18, runbook, registry (stays OPEN).

## Upgrade path to a real channel (kept clean)
Route keeps `channel_kind` + opaque `recipient_ref`; dispatcher resolves channel by kind. Adding
a real kind = one migration (widen CHECKs or introduce alert_channel_kinds, add alert_channels +
credential ref + R2 + R5 + route_changed Kind + receipt_fp) plus the adapter, designed against the
actual vendor. No rewrite of cut-in items.

## What each deferral loses
- Channels/credentials/secret ref: nothing today; moves design to when vendor known.
- Kind overrides: cannot route one Kind differently from its severity.
- R5: last p1 route can be superseded; mitigated by readiness gauge going 0 + audit. Must come back before closure (SR-7 ii).
- route_changed alert: no in-band notice of route edits; audit only. Re-evaluate vs four-eyes for SR-7(i) at closure.
- Enforce mode: platform can run with unready routing; visible via gauge/status only. Human decision H5/H9.
- Dev mock wiring: no manual demo; tests still exercise everything.
- Envs/jitter/extra metrics: tuning needs a code change.
- Receipts: no hand-off proof until adapter.

## Record deferrals (not silently dropped)
Architect should list the DEFER rows verbatim in ADR 0102 §18 "Deferred / future considerations"
(or docs/decisions/), each tagged with its trigger: "real channel kind chosen" (channels,
credentials, receipts, R2, R5, route_changed, conformance extras, N-5), "launch gating decision"
(enforce mode), "first tenant-owned alert Kind" (tenant routing). I did not write to the repo.

## Operator inputs still needed (trim from design §15)
H1 channel vendor/contract, H2 recipients/on-call targets (HD-PRH2-4-OPS), H3-H4 on-call policy
and escalation windows, H6 credential provisioning (only matters once a real kind exists), H8
out-of-band monitor target, H9 whether ALERT-DELIVERY-1 gates money-moving launch, H5 enforce vs
report (deferred but still a decision). H7 (B2B notifications) can wait until a tenant-owned Kind.
ALERT-DELIVERY-1 stays OPEN: no real non-log channel, no configured recipient.
