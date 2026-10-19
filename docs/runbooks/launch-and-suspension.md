# Brand and tenant launch, suspension, reactivation and closure (ADR 0112)

Status: operator runbook for the mechanism of ADR 0112, written against the code at this commit (`internal/launchgov`,
`internal/httpserver/launch_authorisation_routes.go`, migrations 0128-0130, back office `LaunchPanel`). The mechanism is
**IMPLEMENTED against the local database and MOCK providers**; the review conditions recorded in the ADR section 13 notes
are not all closed, and no real brand has been launched through it. Nothing here is a legal or regulatory statement.

> **A recorded decision is not a legal authorisation.** The mechanism records that named humans took a decision on stated
> inputs. It does not determine whether a licence is required or valid for a market, and software readiness is separate
> from legal and licensing approval (CLAUDE.md "Compliance"). Licensing basis, licence references and market choices are
> **human/legal inputs**; this runbook never fills them in.

## 1. Who does what

| Role | May | May not |
|---|---|---|
| `platform_admin` | request (any subject), **approve / reject**, suspend, cancel their own request, read | approve their own request, or approve as the same Person as the requester (two distinct staff rows **and** distinct Persons; a missing Person refuses) |
| `tenant_admin`, `compliance` (tenant scope) | request and **suspend** for their **own tenant's brands**, cancel their own request, read | approve or reject anything; request or suspend a **tenant** subject; touch another tenant |
| `support`, `finance` | nothing (no launch permission in `internal/auth/permission.go`) | any launch action |
| Players, B2C | nothing | n/a |

Permission names: `launch_authorisation:request|approve|suspend|read`. They are static route-level authority, deliberately
not grantable through the capability catalogue and not usable in an acting session. Every write also carries a signed
actor proof (ADR 0110); a signing failure is a 500, never a bypass. The database enforces the transition rules for every
writer: a status change without a same-transaction governed decision is refused (`LA020`), and a tenant or brand created
through the API starts `pending_launch`.

Required approvals (forced by the database, not lowerable by configuration): **1** distinct platform approver for a brand
activate / reactivate / ratify; **2** for a tenant activate, for any close, and for a platform-licence brand activate or
reactivate requested by a tenant-scoped requester.

Requests expire after **72 hours** if undecided. A reject applies to its own request only.

## 2. Lifecycle

```
pending_launch --activate (4E)--> active --suspend (single actor)--> suspended
      |                            |  ^                                 |
      | close (4E)                 |  +------- reactivate (4E) ---------+
      v                            v                                    |
   closed  <------ close (4E) -----+------------- close (4E) ------------+
   (terminal)
```

- **Tenant and brand are separate subjects.** A tenant change never rewrites its brands. A brand can be activated under a
  non-active tenant (staged launch) and has no effect until the tenant is active.
- **`closed` is terminal.** Reopening is refused by the database until HD-CTF-9 (open human decision) is answered.
- `ratify` records licensing/jurisdiction/operator for a row that existed before the mechanism (`legacy_baseline`); it does
  not change status.

What a non-active subject cannot do (all fail closed): register or log in players (`BRAND_NOT_ACCEPTING_REGISTRATIONS`),
start new wagering (tenant gate, and the per-brand gate since migration 0129; casino bets `brand_not_active`/409,
sportsbook `brand_not_active`), initiate payments, and the payments sweeper withholds new money-moving dispatches
(`payments_sweeper_resolution_only_blocks_total`). Terminal stake returns and settlement of already-accepted rounds are
**not** blocked, so existing exposure can finish. Webhooks for a non-active tenant are refused with an audit record
(`casino_webhook_refused_tenant_not_active`, `..._brand_not_active`).

## 3. Launching (activate)

1. **Prepare configuration** (API; nothing here is code): jurisdiction configs in effect (since migration 0177:
   `POST /v1/admin/licensing-changes` kind `tenant_jurisdiction_config_version`, two approvals for a first or widening
   version; `licensing-change-approvals.md`), KYC/AML/RG
   rulesets set, allowed currencies active in the asset registry, payment methods and an approved provider credential
   handle, catalogue enabled, licence record entered by the responsible human. Market rows: `market-blocking-configuration.md`. RG floors and the
   enforcement switch: `rg-enforcement-activation.md`. AML officers: `aml-officer-bootstrap.md`. Containment after launch: `kill-switch-and-containment.md`.
2. **Read readiness**: `GET /v1/admin/tenants/{tenantID}/launch-readiness?brand_id=`. The server computes it; the client
   cannot supply it.
3. **Request**: `POST .../launch-authorisations` with action `activate`, the licensing record, jurisdiction ids, responsible
   operator and `conditions_note`, and the **exact set of non-passing check codes acknowledged** (not more, not fewer). The
   server snapshots and hashes the readiness it bound.
4. **Approve**: a different platform admin (different Person) reads the same readiness and approves, or rejects with a
   reason. The approval carries the readiness hash the approver saw; execution re-checks the hard preconditions.
5. **Execution** happens in the approving transaction when the count is met. Confirm in
   `GET .../launch-status-history` that a `governed` transition exists and the subject is `active`.
6. **Never** take the "owner-provisioned" shortcut (insert a row with `status='active'` as table owner) outside a synthetic
   dev/test database. It exists for fixtures and records an `owner_provisioned` transition; it is not a launch.

### 3.1 What each readiness check means

Each check is `pass`, `fail` or `not_evaluable`; **`not_evaluable` is not a pass**. A non-passing check never blocks by
itself: it must be explicitly acknowledged and the approvers see it. Only the hard preconditions (section 3.2) refuse.

| Code | Passes when | A `fail` / `not_evaluable` usually means | Who resolves |
|---|---|---|---|
| `R-JUR-CONFIG` | each referenced jurisdiction has an in-effect tenant jurisdiction config | no config row, or not yet in effect (`config_missing_or_not_in_effect`), or none referenced | compliance / configuration owner |
| `R-LIC-VALID` | licence-backed model: the recorded licence evaluates as valid at `now` | licence expired, suspended, or the record is for a manual-determination model | **human / legal**; never assumed by software |
| `R-RULESETS` | KYC, AML and RG ruleset ids set for each referenced jurisdiction | `ruleset_unset` | compliance |
| `R-CURRENCIES` | every allowed currency is an active asset in the registry | `currency_missing_or_inactive` | asset registry owner |
| `R-PAY-METHODS` | at least one payment method and an approved provider credential handle exist for the tenant | `no_payment_method` / `no_approved_credential_handle` | payments / provider setup |
| `R-CATALOGUE` | the brand has at least one enabled casino game or sportsbook product | empty catalogue | brand operator |
| `R-MARKET-CONFIG` | at least one country is open for registration, deposit and casino wagering (licence ceiling + tenant opt-in, not narrowed by a brand or operation row) | `no_market_configured`: the tenant never opted a country in; `no_country_permits_all_operations`: configured, but a disabled ceiling, brand or operation row closes every country. Until fixed the market gate refuses every registration, deposit and launch of the brand | compliance (tenant rung) / platform (licence ceiling) / tenant admin (brand rung) |
| `R-PROVIDER-TIER` | the running process uses production-eligible components | `synthetic_environment`: today every provider is a MOCK, so this fails everywhere | provider contracts (human) |
| `R-ALERTS` | **fixed after the A13 audit (finding F1; snapshot version 5).** The alert dispatcher of the running process currently evaluates routing as READY for **both p1 and p2**: the latest current step-0 platform route is **enabled**, names a recipient, uses a human-notification channel kind, and that channel's sink is **wired in this process and itself reports a person** (an email channel over the MOCK provider does not). A human-notification route can only be enabled through SR-7 four-eyes (one admin stages it disabled, a different admin enables it; migration 0156). The channel vocabulary alone no longer passes | `fail`, detail `<p1\|p2>_<reason>`: `no_route_configured`, `route_not_enabled` (disabled, or enabled without a recipient), `channel_not_human_notification` (log, mock, or email over the MOCK provider), `channel_not_wired` (no adapter for the route's kind in this binary). `not_evaluable`: `no_routing_source` (service not wired to a dispatcher), `routing_not_evaluated` (the dispatcher has not yet completed a routing evaluation since start), `routing_evaluation_stale`. A tenant-scoped viewer sees `see_platform`. **Residual (still true on a pass):** routing is platform-wide (there is no per-tenant route), so a pass describes the platform path every tenant's alerts take; it reads configuration only (a vendor outage does not flip it); it does not prove the recipient or the webhook receiver exists or that anyone is on call. ALERT-DELIVERY-1 stays OPEN until a rota is paged. Cross-check with `GET /v1/admin/alerting/status` and `alert-delivery-channels.md` | owner / on-call decision (HD-PRH2-4-OPS); a platform admin pair authors and approves the route |
| `R-AML-QUORUM` | at least two distinct live Persons of the tenant's AML owner scope hold an in-force `aml_officer` grant (`aml-officer-bootstrap.md`) | `quorum_not_met`, `read_error`; for a platform-owned scope the detail carries a platform suffix and a tenant viewer sees `see_platform` | compliance / platform admin |
| `R-AML-SCREENING` | a screening provider is registered (a MOCK counts only outside production) and every referenced jurisdiction has an effective screening policy | provider or policy missing, `read_error` | compliance (policy needs a legal-review reference) / provider contract (PROVIDER DEPENDENT) |
| `R-AML-MONITORING` | **always `fail`** (`monitoring_not_implemented`): transaction monitoring (ADR 0116 S4) is NOT IMPLEMENTED. Informational, not blocking; it must be acknowledged knowingly | n/a | owner (decision to launch without monitoring) |
| `R-RG-CONFIG` | RG limit enforcement is switched ON for the subject (`rg_enforcement_activations`), a compliance floor is in force for deposit, loss and wager in EVERY allowed currency, and no session-duration control is enabled or mandatory (session enforcement is not built) | `enforcement_not_enabled`, `floor_missing_<kind>`, `session_limit_not_enforced`, `session_aggregation_unsupported`, `no_currency` | compliance (floor values, OQ-2) then a tenant compliance staff member (the switch) |
| `R-PLAYER-FUNDS` | (close and suspend) shows balances and exposure; requires acknowledgement | n/a: it is a position, not a pass/fail | approver reads the notice (section 5) |

An approver who sees `R-PROVIDER-TIER` or `R-AML-MONITORING` failing is looking at the true state of the platform. Acknowledging
them does not make them true. `R-ALERTS` passing means a reviewed, enabled, wired human-notification route exists for p1 and p2; it still does
not mean anyone would be paged (see its row). A request filed while `R-ALERTS` failed goes stale when a route is enabled, because the
check result is part of the readiness hash (snapshot version 5): re-request.

### 3.2 Hard preconditions (refusals)

Legal transition from the current state; licensing record internally consistent with the registry (model, licence id,
licensee, determination reference where the model needs one); at least one jurisdiction referenced with an in-effect
config; and for close the funds conditions. A refusal is returned as `LAUNCH_PRECONDITION_FAILED` with check codes, or
another closed `LAUNCH_*` code (`LAUNCH_UNACKNOWLEDGED_CHECKS`, `LAUNCH_SELF_APPROVAL`, `LAUNCH_ILLEGAL_TRANSITION`,
`LAUNCH_SUBJECT_CLOSED`, `LAUNCH_REQUEST_PENDING`, `LAUNCH_RETRYABLE_LOCK_TIMEOUT`, `LAUNCH_WITHDRAWALS_HOLDING_FUNDS`).

## 4. Suspension (single actor, fail-closed direction)

Use it for any suspected integrity, compliance, fraud or licence problem. It is deliberately easy.

1. Anyone holding `launch_authorisation:suspend` for the subject calls `POST .../brands/{brandID}/suspend` or
   `POST .../tenants/{tenantID}/suspend` with a **mandatory reason code**. No second approver.
2. It takes effect in the same transaction and supersedes pending requests for the subject (each superseded request is
   audited; nothing silently disappears).
3. A suspension waits for in-flight gameplay and payment lock holders (a bounded lock timeout); the executor retries a
   bounded number of times. `LAUNCH_RETRYABLE_LOCK_TIMEOUT` means retry, not that nothing happened; check the history.
4. **After suspending, tell the people the system cannot:** players of the subject, the brand operator, and finance. The
   platform sends no player notice for this.
5. Open items after a suspension: withdrawals holding funds continue to hold them and need the separate hold-resolution
   process; in-flight deposits and open casino rounds are not tracked by the funds check; reconciliation keeps observing
   a suspended tenant (a finding there is still a P1). The payments sweeper only resolves, it does not start new
   dispatches.

A tenant-scoped suspender can **never** reactivate alone.

## 5. Funds notices (shown to approvers; fixed texts)

- **Suspension** (`NoticeFundsInaccessibleOnSuspension`): balances become inaccessible for new wagers, deposits and
  withdrawals until reactivation through a new four-eyes decision. Funds are not stranded; they stay in the ledger
  unchanged; hold-bearing withdrawals continue to hold their funds.
- **Closure** (`NoticeFundsStranded`): closing with non-zero player balances **strands** them. Closure is terminal and the
  unclaimed-funds / dormant-balance policy is OPEN (HD-CTF-4), so the funds stay in the ledger with no process to return
  them. Acknowledging `R-PLAYER-FUNDS` acknowledges this. Do not close a subject with balances without a human decision on
  that policy.
- **Always** (`NoticeFundsOmissions`): the check does not see in-flight deposits or open casino rounds (Q-GP-6); only
  wallet balances, open sportsbook bets and hold-bearing withdrawals are counted.

## 6. Reactivation and recovery

1. Resolve the cause. Reactivation is **always four-eyes** with a **fresh readiness snapshot** (a request, a distinct
   platform approver). It is not undone suspension; it is a new launch decision.
2. Before approving, confirm: the reconciliation cycle is clean for the tenant (no open mismatch), the kill switch state is
   deliberate (`payment-kill-switch-release-recovery.md`), and queued work behaves as expected (payments sweeper
   `resolution_only_blocks` should stop growing; KYC outbox rows deferred for an inactive tenant drain).
3. After reactivation expect backlog: the sweeper resumes dispatches, the KYC outbox sends deferred rows, webhooks that were
   refused during the suspension are not replayed by the platform (vendors may retry). Reconcile statements for the gap.
4. If the subject is `closed`: it cannot be reopened (HD-CTF-9). Create a new subject through the normal flow.

## 7. Evidence and audit

Every decision writes an append-only record: requests, approvals (with the readiness hash at decision), status
transitions, and the audit row. The tenant-visible history is `GET .../launch-status-history`; the platform-wide queue is
`GET /v1/admin/launch-authorisations`. Keep the request id in the incident ticket.

## 8. What is NOT in place

Real providers (all MOCK); a real alert recipient (ALERT-DELIVERY-1); licence continuous-expiry auto-flagging; a legal
determination of any kind; the unclaimed-funds policy (HD-CTF-4); reopening a closed subject (HD-CTF-9). The ADR section
13 notes list the remaining review conditions.
