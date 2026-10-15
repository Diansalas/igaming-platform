# Incident response and on-call: TEMPLATE runbook

Status: **TEMPLATE. No on-call rota, no recipient, no paging channel and no escalation contact exists.** ALERT-DELIVERY-1 is
OPEN and HD-PRH2-4-OPS (who is on call) is an unanswered human decision. Every `<...>` below is a blank for a human to
fill; this document invents no name, address, phone number, tool or SLA. Until the blanks are filled, an incident is
discovered by a person looking, not by a page (`observability-and-alerting.md`, `deploy/observability/`: the metric rules are
backed by a real exposition since A9 but no Prometheus or pager is deployed; the log-event rules are not wired).

## 1. Fill-in section (owner / operations; HUMAN)

| Item | Value |
|---|---|
| Incident commander pool | `<NOT ASSIGNED>` |
| Primary on-call (per rota period) | `<NOT ASSIGNED>` |
| Secondary / escalation | `<NOT ASSIGNED>` |
| Database authority (may run privileged DB steps) | `<NOT ASSIGNED>` |
| Finance / ledger authority | `<NOT ASSIGNED>` |
| Compliance / legal contact (licence, regulator, player notices) | `<NOT ASSIGNED>` |
| Security contact | `<NOT ASSIGNED>` |
| Paging channel (a person actually notified) | `<NOT ASSIGNED>` (closes ALERT-DELIVERY-1 together with a real route in `/v1/admin/alerting/routes`) |
| Incident channel / ticket system | `<NOT ASSIGNED>` |
| Status communication channel for tenants / players | `<NOT ASSIGNED>` |

Record the date each blank was filled and by whom. Do not put credentials, phone numbers of private persons or secrets in
this repository; reference the system of record instead.

## 2. Severity (aligned with `internal/alerting`: `p1` and `p2`)

| Sev | Meaning | Examples in this platform | Expected response |
|---|---|---|---|
| P1 | Player money or integrity at risk, or a compliance control failed open | any non-zero ledger drift (CLAUDE.md: always P1); readiness failing for all traffic; DB loss; payment kill switch engaged unexpectedly; webhook admission rejecting everything; suspected compromise; a restore needed | immediately; commander named; humans notified |
| P2 | Degraded or at-risk, money safe | one provider down with cascade working; payments sweeper stalled; KYC outbox lagging; 5xx ratio elevated for one route | same day; owner named |
| P3 | Defect or hygiene | single failing background item, capacity refusals | ticket |

If in doubt, treat it as the higher severity and downgrade later.

## 3. Roles for a P1 / P2

- **Incident commander (IC):** owns decisions and the timeline; does not debug.
- **Operator:** hands on the systems; states every command before running it; for privileged database steps the operator
  is the database authority, never an automated agent against a shared or production database.
- **Scribe:** timestamped log of observations, decisions and commands.
- **Communications:** one voice for tenants / players / regulator-facing statements (content approved by compliance).
- **Finance / ledger authority** joins for anything touching balances, holds, settlements or reconciliation.

One person may hold several roles in a small team; the IC and the operator should differ for P1.

## 4. First 15 minutes (no monitoring is wired: check by hand)

1. Declare: write the time, who noticed, what they saw, and the first severity guess.
2. **Is it serving?** `GET /healthz` (liveness) and `GET /readyz` (DB ping, webhook directory loaded). Both are cheap.
3. **Is it failing?** Request log lines are JSON with `msg="http_request"`, `status`, `duration_ms`, `request_id`,
   `tenant_id`. Count `status >= 500` over the last minutes; look for `panic_recovered`.
4. **Money check (always, for P1/P2 on payments, ledger, settlement):** look for `reconciliation sweep: MISMATCH FOUND`
   in logs; platform admins list durable alerts via `GET /v1/admin/alerts` and routing state via
   `GET /v1/admin/alerting/status`. Reconciliation staleness has a metric since A9 (`reconciliation_last_success_unix_seconds{stream}`, oldest tenant; read it at
   `METRICS_ADDR`). The SQL check still works without it: per tenant, with the tenant scope set,
   `SELECT stream, max(run_at) FROM reconciliation_runs GROUP BY stream` (default cadence is hourly; older than two hours
   is a stalled scheduler).
5. **Background loops** (payments sweeper, KYC outbox, alert dispatcher, reconciliation) run inside `platform-api`; their
   state is in the `payments sweeper:` / `kyc outbox` / reconciliation log lines. Procedures: `operational-runbooks.md`
   sections 12 and 13.
6. **Containment options, in increasing impact:** payment kill switch for a provider / operation
   (`payment-kill-switch-release-recovery.md`); suspend a brand or tenant (`launch-and-suspension.md`, single actor);
   fence traffic at the load balancer; stop `platform-api` (fails closed by design).
7. Write the severity again; page/notify the people in section 1 that exist.

## 5. Symptom to procedure

| Symptom / alert rule (not wired) | Go to |
|---|---|
| `LedgerDriftP1`, MISMATCH FOUND, projection != ledger | `operational-runbooks.md` section 3. **Never UPDATE a balance or ledger row.** Recompute the projection only if entries are intact; otherwise ledger-finance |
| `ReadinessFailing`, `/readyz` 503, DB unreachable | `operational-runbooks.md` section 7; after any recovery re-verify role separation, RLS and TEMP revoke; `restore-from-backup.md` if data loss |
| `Http5xxRatioHigh`, `PanicRecovered` | logs by `request_id`; roll the application back (previous build is safe by expand/contract) |
| `PaymentsSweeperStalled`, `PaymentsSweeperItemFailures` | `operational-runbooks.md` section 12 |
| `PaymentKillSwitchEngaged` | `payment-kill-switch-release-recovery.md`; find who engaged it (`changed_by`) before releasing |
| `WebhookAdmission*` | `operational-runbooks.md` section 4/5 and `docs/decisions/0097-*.md`; `directory_unloaded` means failing closed on every webhook route |
| `KycOutbox*` | `operational-runbooks.md` section 13 |
| `AlertRoutingNotReady`, `AlertUnrouted`, `AlertDeliveryDead` | this is ALERT-DELIVERY-1: nobody was or will be notified; escalate to the owner (human decision), do not silence |
| `DbPoolSaturated`, `DbPoolAcquireWaiting` (metric exists since A9, not wired to a pager) | `observability-and-alerting.md` section 4; symptoms: rising request duration then 5xx; connection counts in `pg_stat_activity`; `operational-runbooks.md` section 7 step 3; `load-and-soak-test-plan.md` |
| `ReconciliationStale`, `ReconciliationSweepStalled`, `ReconciliationMismatchesOpen`, `ReconciliationListingFailed`, durable alert `reconciliation.sweep_failed` | `observability-and-alerting.md` section 4 (runbooks for `reconciliation.sweep_failed` and `run_failed`); `operational-runbooks.md` section 3 |
| `AlertUndeliveredP1Old`, `AlertDeliveryFailing`, `AlertRetention*` | `alert-delivery-channels.md`; undelivered P1 = nobody was told: ALERT-DELIVERY-1, do not silence |
| Provider outage | `operational-runbooks.md` sections 4 and 5 |
| Suspected compromise / secret exposure | `operational-runbooks.md` section 9 and 15 (actor proof keys); rotate, do not paste secrets anywhere |
| Need to restore | `restore-from-backup.md` (human decision; section 6 there) |
| A brand must stop now | `launch-and-suspension.md` section 4 |

## 6. Rules during an incident

- Fix forward with compensating entries; no edits or deletes of ledger history; no hand-edits of `schema_migrations`.
- No privilege escalation, role or password changes to "get access"; if access fails, stop and report.
- Do not force a payment or deposit to `settled` without provider confirmation; use the governed force-resolution path
  (`operational-runbooks.md` section 14).
- Do not paste secrets, tokens, PAN-like data or personal data into tickets, chat or logs.
- Anything that looks like a licensing, legal, regulator-reporting or player-notice question goes to the compliance /
  legal contact; the software gives no legal advice.
- Production credentials are requested only by a human with authority; an agent never requests them.

## 7. Communication template

```
[<time UTC>] [P<n>] <one-line impact in plain words>
Scope: <tenants/brands/providers affected, or "under investigation">
Player money: <safe / at risk / unknown> (basis: <reconciliation clean as of ...>)
Actions taken: <containment steps with times>
Next update: <time>   Owner: <role, not a private contact>
```

Statements to tenants or players are approved by compliance. Do not state a cause that is not yet established.

## 8. Post-incident review (blameless; within `<NOT DEFINED>` days)

1. Timeline from the scribe log (detection, declaration, containment, resolution).
2. Impact: money (reconciled?), players, tenants, duration, data exposure.
3. Why was it not detected earlier: which rule in `deploy/observability/` would have fired, and was it wired?
4. Root cause and contributing factors (lock ordering, idempotency, provider double-callback are the known historical ones).
5. What worked, what did not, what was luck.
6. Actions: owner, due date, tracking id. Any new alert signal gets a rule and a parity-tested entry.
7. Whether a runbook or an ADR needs a change; whether the incident touches CLAUDE.md rules (record a decision if so).
8. For a P1 involving ledger or credentials: ledger-finance and security review the review.

## 9. Drills

Practice before it matters: `restore-from-backup.md` (local drill), a kill-switch engage/release rehearsal on a scratch
database, and a tabletop of section 4 with the blanks filled in. A rota that has never been exercised is not a rota.
