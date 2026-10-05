# Security design review: ALERT-DELIVERY-1 delivery activation (planned migration 0117)

- Reviewer: security specialist. Date: 2026-10-05. Base: HEAD `95b17c5`.
- Under review: `scratchpad/alert-delivery-design.md` (architect, DESIGN/PROPOSED).
- Mode: read-only, design level. Nothing was built, run or probed. No tests, DB or builds. This is not a pen test.
  What I checked against code: `migrations/0110_durable_alerting.up.sql` (functions, tables, RLS, grants),
  `0114` (Kind seed and unseed pattern), `0115` (search_path pins), `internal/alerting/dispatcher*.go`,
  `internal/db/platform_service.go`, `internal/httpserver/alert_admin_handlers.go`, `capability_routes.go`
  (denied-audit pattern), `internal/auth/permission.go` + `jwt.go` (roles), `internal/secretstore/{secretstore,fetcher}.go`,
  `devfile/devfile.go`, `internal/providerref`, `internal/providerkind/guard.go`, `cmd/platform-api/{main.go,registrations.go,alert_dispatcher_wiring_test.go}`,
  ADR 0102 §4.2/§6.2/§17.7, the runbook §3, `docs/security/runtime-role-separation.md`, the iwire security records,
  and registry rows TRIGGER-SEARCH-PATH-1 / -UPGRADE.

## VERDICT: ACCEPT WITH CONDITIONS

The design is sound overall. It is fail-closed by default (`enabled DEFAULT false`, nothing seeded, no
`human_notification` kind). It keeps AL-10 and AL-5. It fixes N-4 visibly. It keeps routes and channels
platform-only. It is honest that ALERT-DELIVERY-1 stays OPEN. Two findings are HIGH and must be fixed in the
design before implementation starts: H-1 (search_path/TEMP) and H-2 (SR-7 governance for real channels).
The MEDIUM findings must also be fixed before the change is marked complete. I **decline** S1's dev-profile mock wiring.
I **reject** "`alerting.route_changed` instead of four-eyes" as enough for a REAL channel. I **accept** it for
the log/mock phase only under the conditions in H-2 and M-4.

Launch flag (unchanged): ALERT-DELIVERY-1 remains a launch blocker for money-moving flows that rely on these
P1s (human decision, H9). After this workstream no person is notified of anything.

---

## Findings

### H-1 HIGH: search_path/TEMP hygiene is missing from the design, and 0117's new guards would sit on unpinned 0110 functions
Evidence: none of the 0110 functions has `SET search_path`. That covers `alerting_validated_platform_admin` (0110:33-45, an
unqualified `staff_users` lookup), `alerting_session_scope`, `alerts_guard`, `alert_occurrences_guard`, `alert_routes_guard` and
`alert_deliveries_guard`. 0115 pins all 18 of its functions. Registry TRIGGER-SEARCH-PATH-1-UPGRADE (HIGH, launch blocker)
reproduced a TEMP-table shadow bypass of the K2 two-person check by `igaming_runtime`, which has TEMP via PUBLIC.
The design never mentions search_path.
Failure scenario: an attacker with arbitrary SQL as `igaming_runtime` (SQL injection or a compromised app) runs
`CREATE TEMP TABLE staff_users(...)` with an invented platform admin. They can then pass the trigger's
`alerting_validated_platform_admin()`, which forces `created_by` on routes and channels and acts as the ack/resolve actor. With
`CREATE TEMP TABLE alert_channels/alert_routes` they also shadow the lookups in the new R2/R5 triggers. That lets them enable a route
whose real channel is disabled, or supersede the last p1 route while a fake temp "replacement" satisfies R5.
(RLS policy expressions are bound to OIDs at CREATE POLICY time, so the policies themselves are not shadowable. The trigger functions are.)
Required:
1. Every function and trigger function that 0117 creates or replaces carries `SET search_path = pg_catalog, public, pg_temp`.
   That includes the R2, R5, activation and channel guards, any helper, and the replaced `alert_routes_guard`.
2. 0117 adds `ALTER FUNCTION ... SET search_path = pg_catalog, public, pg_temp` to the 0110 functions its new controls
   depend on: `alerting_validated_platform_admin`, `alerting_session_scope`, `alerts_guard`, `alert_occurrences_guard`,
   `alert_routes_guard`, `alert_deliveries_guard` and `alert_kinds_deny_write`. This is in scope: it hardens the
   tables this migration changes, and it does not decide the systemic TRIGGER-SEARCH-PATH-1 fix for other domains.
3. Add TEMP-shadow probe tests (same shape as the 0114/0115 probes): a temp `staff_users`, `alert_channels` or `alert_routes`
   does not change the outcome of created_by forcing, R2 or R5. Add a catalog test that every `alert%`/`alerting%` function
   has `proconfig` containing the pin.
4. Down migration: `ALTER FUNCTION ... RESET search_path` is acceptable for chain-test symmetry. Leaving the pins in place is preferred.

### H-2 HIGH (binding before any real channel; structural guard required now): `alerting.route_changed` alone is not sufficient governance for a real channel
Facts: `RolePlatformAdmin` is the only platform-scoped role (`jwt.go:36-52`). It will hold `alert:route_manage`
and `alert:manage` (ack/resolve). So one person can change routing and also ack or resolve the alert that reports the change.
Failure scenarios (the insider threat model is "silence the ledger-drift / multiple-success P1 while committing
fraud". These are the exact P1s whose paging K2's four-eyes relies on):
(a) The `route_changed` notification is delivered through the newly written routing, which may point to a target the
    attacker controls. It never reaches the previous recipient.
(b) The design sets no discriminator for `route_changed`. With a constant discriminator, every later change
    dedups into the same open or acked alert as an extra occurrence. Delivery is per alert, not per occurrence, so after one
    benign change plus ack, later malicious changes produce **no** delivery at all.
(c) The design's §10/§13 makes the raise non-fatal (the savepoint rule, and the test "route write commits even if the raise
    fails"). The SR-7(i) control can therefore fail silently.
(d) R5 checks only that some enabled human p1 route exists. It does not check where it points. Redirecting the last p1 route to another recipient
    passes R5.
Ruling:
- **Log/mock phase (this workstream): an audited `route_changed` is sufficient**, because no route reaches a human, so there is
  nothing to divert. Conditions: M-4 (fail-closed raise, per-version discriminator), plus the structural guard below.
- **Before any `human_notification=true` kind: four-eyes is required.** Use a distinct-Person request/approve, as in the
  existing `provider_credential:request/approve` and K1 `capability_grant` patterns, which are DB-enforced. It applies to (i) enabling or superseding any
  p1/p2 route, (ii) any channel version that is enabled or that changes `credential_secret_ref`, and (iii) supersession of the last
  p1 route. Also, `route_changed` must additionally be delivered to the *superseded* route's recipient (the previous owner of the
  pager), or the out-of-band monitor must alert on `alert_route_changes_total`.
- **Structural guard in 0117 (required now):** the route/channel guard triggers refuse `enabled = true` for any
  route or channel version whose `alert_channel_kinds.human_notification = true`, with error "SR-7: four-eyes approval
  required; not built". The migration that first seeds a real kind must consciously replace this together with four-eyes.
  This turns "four-eyes before a real channel" from a registry note into a database fact. Consequence: the
  readiness test that uses a "test-only fake human_notification kind" (§13) must test the evaluator in Go, or must
  insert the fake kind and route in a throwaway scratch DB with the guard explicitly bypassed in that scratch DB only. See L-3.

### M-1 MEDIUM: kind-specific route overrides allow targeted silencing that readiness and R5 cannot see. CUT recommended
`kind` overrides win over severity defaults (§7.1). Readiness and R5 count only `kind IS NULL` routes (§7.8, §4.3).
Failure scenario: one admin action adds an enabled override for `reconciliation.ledger_projection_drift`, or for
`alerting.route_changed` itself, pointing to a `log` channel. That Kind is then never seen by a human. `alert_routing_ready`
stays 1, R5 is satisfied, and no `unrouted` signal fires because the log channel reports `sent`.
Required: **cut the `kind` column and overrides from this round.** There is no Blueprint or registry requirement for them, and they add
mutation targets and readiness subtlety. If the owner insists on keeping them: refuse overrides for `alerting.*` Kinds, and require an
override for a p1/p2 Kind to target a `human_notification` channel. Overrides then fall under the H-2 four-eyes too.

### M-2 MEDIUM: R5 (last p1 route) has four bypass or correctness gaps
(a) **Channel kind swap:** R5 on channels allows superseding with "a replacement version that is itself enabled". A
    replacement with the same `channel_key` but `channel_kind='log'` passes, and every route on that key silently becomes non-human.
    Required: `channel_kind` is immutable across versions of a `channel_key`. A different kind needs a new key.
(b) **Ordering:** supersede-then-insert, needed because of the R3 partial unique index, which cannot be DEFERRABLE, means a row-level R5 at
    UPDATE time sees zero routes and wrongly refuses. Required: implement R5 as a `CREATE CONSTRAINT TRIGGER ... DEFERRABLE INITIALLY
    DEFERRED`, evaluated at commit and pinned per H-1.
(c) **Cross-table write skew:** under READ COMMITTED, tx1 moves the p1 route from channel X to Y while tx2 disables Y. Each
    commit-time check sees the other's pre-image, both commit, and the last p1 route ends up on a disabled channel. Required: every
    route and channel write serialises on one transaction-scoped advisory lock (a fixed key), taken **inside** the guard trigger,
    not by Go discipline. Add a two-session concurrency test.
(d) **p2 vs `enforce`:** readiness counts p1 and p2, but R5 protects only p1. Under `ALERT_ROUTING_READINESS_MODE=enforce`, one
    admin action (supersede the last p2 step-0 route with a disabled version) makes `/readyz` 503 on every pod. That is a
    whole-platform outage, including RG self-exclusion and withdrawals. Required: either R5 covers every severity that readiness
    counts, or `enforce` counts only p1. See also L-6.

### M-3 MEDIUM: log/non-human "sent" is presented as `delivered`, and a log route in production hides the unrouted signal
The view, status counts and `alert_open{delivery_state}` map `sent` to `delivered` regardless of channel kind (§4.6). The status
API would show P1s as "delivered" when only a log line was written. That breaks the owner constraint "log output never counted as human
notification" at the API level. Also, today every alert in production is visibly `unrouted` (meta and metric). Enabling a `log`
route for p1/p2 makes those alerts `sent` and suppresses the unrouted meta and metric. That is a visibility regression, and only the readiness
gauge would still show it.
Required: (1) the derived state is `delivered` only when the channel kind is `human_notification`. Otherwise use
`recorded_non_human` (or a similar name), and metrics carry the same distinction. (2) In production, refuse enabling p1/p2 routes on a
non-`human_notification` kind. Enforce this in the HTTP layer and test it. A `log` route stays allowed for p3 and outside production.

### M-4 MEDIUM: the `route_changed` raise must be fail-closed and must open a new alert per change
The ADR 0102 §7.2 savepoint/swallow rule exists so an alert failure never rolls back a **financial** transaction. A route or
channel write is administrative configuration, so that rationale does not apply, and swallowing the raise defeats SR-7(i).
Required: (1) Raise with strict `Raise` in the same platform-admin tx. If the raise fails, the route or channel write rolls back (fail
closed), and the 5xx is audited as a failure in a separate denied/failure audit tx. Invert the §13 test: a forced raise failure means no
route row and no `route_version_created` audit row. (2) Discriminator = `route:<new_version_id>` or `channel:<new_version_id>`, so every
change opens its own alert and gets its own step-0 delivery. Today a step-0 delivery happens even if the alert is acked first, because `readDueWork`
treats "no delivery row" as due regardless of ack (dispatcher.go:352-353). Keep that behaviour pinned by a test. (3) `allowed_keys`
values are closed tokens validated in Go before SQL.

### M-5 MEDIUM: panic and error logging must not carry adapter values now that `Send` receives a credential
`runPassRecovered` logs `fmt.Sprint(r)` (dispatcher_loop.go:132). The new per-alert recovery would likely copy that. A real
adapter that panics with an `error` that wraps an HTTP request, URL (tokenised webhook), response body or header would put
credential or body bytes into logs. `secretstore.Secret` redacts itself (secretstore.go:69-81), but derived strings do not.
Required: per-alert and per-pass recovery log only the panic's Go type (`%T`) plus alert id, step, attempt and channel_kind, never the value.
Adapter-returned errors are reduced to `ErrorClass`; no error text is logged or persisted. The conformance suite must include a
panicking adapter that carries a canary secret, and assert the canary is absent from captured logs. Mutation target: the per-alert recover
logs `%v`.

### M-6 MEDIUM: the secret-ref namespace rule must be structural, not a substring test. Recommend deferring the credential path
"Must contain `/platform-alerting/` and must not contain `/provider-creds/`" (§4.2) is weaker than the existing
`Ref.InNamespace` rule (secretstore.go:256-296: the marker appears exactly once, there is an exact segment count, and each segment matches
`refNameSegment`, which refuses `..`).
Required if the credential path is kept: (1) the DB CHECK and Go rule both require the marker exactly once, followed by exactly one
`refNameSegment` that **equals the row's `channel_key`**. That binds the credential to its channel and stops one channel borrowing another's
secret. (2) Go pins the awssm ARN partition, region and account to configured platform values, so a cross-account ARN (a secret in an account
the attacker controls, readable by the platform role through a resource policy) is refused. Production IAM uses enumerated ARNs
(`deploy/aws/modules/iam/main.tf:111-117`), which is the second layer. H6 must add a separate `.../platform-alerting/*` grant. (3) Fingerprinting
uses the providercred keyed `FingerprintKey` with a domain separator (for example `"alerting-channel-credential:"`). (4) Note: the
`devfile` backend serves only `provider-creds/<tenant>/...` (devfile.go:105-111), so a devfile `platform-alerting` ref can never
resolve. Dev and test can use `memory://` only, unless the backend is changed, and that change needs its own security review.
**Recommendation:** no kind in 0117 has `requires_credential=true`, so the write-time fetch/fingerprint handler path and
the dispatcher Fetch are dead code in production this round, exercised only by fakes. Keep the columns, the CHECKs (with fix (1)) and the
pairing CHECK. **Defer** both fetch paths to the real-adapter workstream, which will also bring four-eyes (H-2).

### M-7 MEDIUM (ADR text now, binding before a real adapter): `recipient_ref` must be neither a credential nor a network address (SSRF/injection)
The 0110 shape `^[a-z0-9][a-z0-9_.:-]{0,127}$` accepts a 32-hex lowercase vendor routing key (credential-equivalent:
whoever has it can raise incidents). It also accepts `169.254.169.254:80` / `10.0.0.5:8080`. `LogSink` logs `recipient_ref`, and GET routes show it.
Required ADR §18 rules plus conformance-suite cases:
(1) A credential-bearing address (routing or integration key, tokenised webhook URL) lives only in the secret store, never in
`recipient_ref`.
(2) Adapters never resolve, dial or interpolate `recipient_ref` as a host, URL, path, header or template. It is a vendor
target id passed as escaped data.
(3) A real adapter's endpoint is a compiled-in vendor host allowlist: no redirects followed, TLS verified, private/link-local/
metadata ranges refused after DNS resolution. Neither the secret value nor the DB can supply a base URL.
(4) A generic "webhook" kind is out of scope without its own egress-allowlist security review.
(5) Attribute rendering stays escaped (S-3 precedent).
Recipients remain human-configured and never seeded (R4 is correct).

### L-1 LOW: refusal audit must reuse the K1 denied-audit pattern
Auditing 403/404/409/422 (§10) is correct and closes the iwire follow-up. Write denied rows in the **caller's own scope**
(`capability_routes.go:103-116`: tenant staff go into their own tenant's audit, platform admins into the platform audit),
with `deniedAuditCtx` bounding the work. Never write a `TenantID=Nil` row from a tenant session. A 401 is not audited (metric and log only).
`RequirePermission` refuses before the handler runs, so the permission check must move into the handler, as K1 does, for the
denied row to exist. Audit metadata fingerprints of `recipient_ref` must be keyed (an unkeyed `providerref.Fingerprint` of a low-entropy
opaque ref can be reversed by dictionary). A secret ref is a staff-visible handle and may be fingerprinted with a keyed hash or omitted.

### L-2 LOW: the down migration can fail open, and the Kind unseed order matters
After a down, any route row that 0117 left `enabled=false` (including pre-0117 dev rows that 0117 disabled) becomes **live**
under 0110 semantics, because 0110 has no enabled flag. Required: refuse down if `alert_routes` has any row at all (not only rows with
`channel_key` or `kind`). Remove the `alerting.route_changed` Kind row with the 0114-down pattern (disable the deny trigger, add a temporary literal
DELETE policy, then re-enable, all inside one DO block) **before** restoring the 0110:141-144 CHECK. Otherwise the CHECK re-add fails. The rest of §4.7
is correct and must still succeed on an empty scratch DB.

### L-3 LOW: `alert_channel_kinds` grants and test fixtures
Required: `REVOKE ALL; GRANT SELECT` to `igaming_runtime` in the 0110 §7 style, and no INSERT policy. The design omits these grants.
The "test migration fixture" that inserts a fake human kind must never touch the shared test DB (CLAUDE.md: no sub-agent alters
global test infrastructure). A stray `human_notification=true` row would also make readiness tests elsewhere pass falsely. Prefer a
Go-level evaluator table, or a per-test scratch DB that is dropped afterwards.

### L-4 LOW (LF to confirm): §7.7 wording is inaccurate
"`alert_dispatcher` has no grant on any financial table" is wrong. The dispatcher is the shared `igaming_runtime` role with a GUC, and
its reach is bounded by RLS predicates, not grants. The 0114 KYC worker needed 36 fence policies because a no-tenant session
can reach more than reference data on some tables (`platform_service.go:45-58`). The post-commit guarantee itself holds structurally: separate
short txs, a separate goroutine, `txscope.Held` plus the Fetcher guard, and no business tx awaits delivery. Reword §7.7 to say that.

### L-5 LOW: the DB third layer does not see `alert:route_manage`
`alerting_validated_platform_admin()` checks platform scope, not permission. The DB layer therefore allows any platform staff user, and the
permission is enforced only in Go. This is acceptable while `platform_admin` is the only platform role. Pin each layer separately (S-6 precedent). Handlers must
also refuse acting/grant-elevated contexts, which the DB already excludes via `app.acting_*`. Register `alert:route_manage` with K1 in
`permission.go`, add it to the permission/role coverage tests, and grant it to `RolePlatformAdmin` only.

### L-6 LOW (input to H5): readiness `enforce` scope and staleness
Readiness must depend on configuration only, never on live channel success. Otherwise a vendor outage becomes a platform outage, so state
this explicitly. A cached evaluation older than about 3x the dispatch interval counts as not ready under `enforce`. Fail-closed refusal of an unset mode in
production is correct. Security input for H5: whole-service `/readyz` coupling has a large blast radius, including RG self-exclusion.
A narrower gate, such as refusing money-moving flows or a startup gate, deserves consideration. That is a human decision.

### L-7 LOW: secret fetch per attempt
Rely on the Fetcher cache. Put Fetch inside the same `ClaimLease/2` budget as `Send`. The `uuid.Nil` tenant gives alerting its own
breaker/admission bucket (`MaxConcurrentStoreCallsPerTenant=2`); document this. (Moot if M-6's deferral is taken.)

### INFO
- **AL-10 departure (derived `delivery_failed`): ACCEPTED.** Storing the state in `alerts.state` would require dispatcher UPDATE on `alerts`, which
  AL-10 forbids. Keep it derived. Owner visibility is equal (status API, metric, `delivery_dead` meta).
- **View `alert_delivery_status`:** `security_invoker = true` is essential. A non-invoker view owned by a superuser or BYPASSRLS
  migration role (common in dev) would bypass RLS entirely. With invoker, a subject tenant sees its subject alerts with no
  delivery rows, so it would see a misleading `pending`. **Recommend CUT:** compute the state in a constant SQL query inside the
  platform-admin handler (`WithPlatformAdmin`). This removes one DB object and one mutation target. If kept: it is never read from a tenant session, it is
  tested for that, and the "view loses security_invoker" mutation stays.
- **Meta loop / amplification verified:** meta raises dedup on `severity:<p>` (dispatcher_actions.go:197), `IsMetaKind` stops
  recursion, the unrouted meta is RETURNING-gated, and `route_changed` is non-meta so it may raise `delivery_dead`, which is bounded. With M-4's per-change
  discriminator, `route_changed` volume is bounded by admin actions. Retry budgets are acceptable as technical defaults (T1). A Send that
  ignores ctx still blocks the pass, so the conformance ctx test plus the external "flat `alert_dispatcher_passes_total`" alert are the cover.
- **Dead is terminal:** after a channel fix, alerts that died while it was misconfigured are not re-delivered. The runbook must direct operators to
  the status/list endpoint.
- **N-5:** registering channels in `buildRegistrations` (LogChannel `ProductionEligible`, RecordingChannel `Synthetic`) closes the
  production-refusal gap at startup. The runtime `channel_not_eligible` check is redundant but cheap. Keep it.
- **Migration number:** 0116 does not exist at `95b17c5`. The orchestrator should confirm the 0117 allocation.

## S1 rulings
1. **`ALERT_MOCK_CHANNEL` dev-profile wiring: DECLINED.** It reverses a pinned I-wire invariant ("MockSink never wired"), adds a
   cmd import exception for a test package, and has no consumer except manual dev runs. It would also write real `sent` rows
   that look like delivery in dev DBs (fake-completion evidence). Keep `RecordingChannel` in `alertingtest`, tests-only. Keep and extend
   `alert_dispatcher_wiring_test.go`. `GuardEnvironment()` treating a missing APP_ENV as production would have been correct, but it is not needed.
2. **`route_changed` instead of four-eyes:** sufficient for the log/mock phase only (with M-4). Not sufficient before a real
   channel. Four-eyes is required then, and the 0117 structural guard (H-2) enforces it.

## What to CUT from this round
1. Dev-profile mock channel wiring (S1 declined).
2. Kind-specific route overrides (`alert_routes.kind`) (M-1).
3. `alert_delivery_status` view: replace with a handler query (INFO; recommended).
4. Write-time secret fetch/fingerprint and dispatcher credential Fetch: keep columns and CHECKs only (M-6; recommended).
5. Optional (security-neutral, product-owner call): p1 escalate-on-dead. It has no effect while no human channel exists.
Keep: R1-R5 (with M-2), the structural SR-7 guard (H-2), N-4 unrouted reasons, readiness gauge/status, refusal audit (L-1),
ack reason, `alert:route_manage`, `alertingtest` conformance suite (with M-5/M-7 cases), and buildRegistrations/N-5.

## Minimal required changes to the design before implementation
H-1 (pins, the 0110 ALTER FUNCTION pins, TEMP probes); H-2 (structural refusal of enabled human kinds plus ADR §18 four-eyes
precondition); M-1 (cut overrides); M-2 (kind immutability, deferred R5, advisory-lock serialisation, p2/enforce alignment);
M-3 (`delivered` only for human kinds; no p1/p2 non-human routes in production); M-4 (fail-closed raise, per-version discriminator);
M-5 (type-only panic logging, canary test); M-6 (structural namespace + channel_key binding + account pin, or defer the
credential path); M-7 (recipient_ref rules in ADR plus conformance cases). LOW items may land in the same branch, with L-2 and L-3 before merge.

## Scope of this review (what was NOT covered)
This review covers the design only. No implementation exists to review, and no probe was run. It did not cover real adapter code, vendor
selection or data-location (H1), ALERT-RETENTION-1, the systemic TRIGGER-SEARCH-PATH-1 fix outside the alert tables, or the IAM
policy for the `platform-alerting` secrets (H6). The implementation needs its own security review, including a re-check of
the TEMP-shadow probes and the wiring test. Any later real channel needs a separate review under ADR 0102 §17.7.
