# RV-PRH-I1 (kill switch) — Security review of the payment kill switch

- Reviewer: `security` specialist
- Date: 2026-09-27
- Subject: ADR 0095 §10.2, §10.2.1, §10.2.2, §10.3, §10.5 and the §10.6 implementation record. Round 1: `c5a744b`, `70f2434`, `7c0077e`, `eafddea`. Phase 1: `8d82302`, `08b4391`, `a726827`, `d8e347b`, `1efdef5`. Reviewed at `be0d899`.
- Verdict: **CHANGES REQUIRED.** The authority model holds under adversarial probing:
  - actor and scope forcing;
  - tenant and platform RLS;
  - platform lock;
  - four-eyes in the same transaction;
  - no DELETE;
  - immutable scope.

  The route deviation is **accepted with conditions**. PRH-I1 **must not be marked complete** until H1 is closed and ledger-finance H3 is fixed at all three call sites listed under "Fail-safe" below. M1 to M3 and M6 must be closed before the stage gate. M4, M5 and a real S95-C7 alert are **launch-blocking**.

## Scope and method

In scope:
- `migrations/0105_payment_kill_switch.{up,down}.sql`.
- The `deploy/init-app-role.sql` grants.
- `internal/payments/killswitch.go`.
- The `attempt.go` claim predicates for T2, T1+T2/T1p, T12 and cascade T1.
- `internal/httpserver/payments_kill_switch_handlers.go`.
- The `internal/auth/permission.go` grants.
- The OpenAPI paths.
- `capability.go` `validateManifest`.
- `internal/testsupport/credentialscan` and the three `credential_reflection_test.go` files.

Out of scope:
- Phase 2 (orchestrator T3/T1p wiring, PROV-OUTBOUND-CRED-1 kind-split).
- The callback cutover itself (see `rv-prh-i1-callback-ledger.md`).
- Penetration testing.

This review does not declare the kill switch "secure" in general.

Method:
- I used a detached worktree at `be0d899` and a private database `secrv_ks_i1`. I created it via `TEST_ADMIN_DATABASE_URL`, migrated it to 0105, and applied the `deploy/init-app-role.sql` grant tail. Afterwards I dropped the database and removed the worktree.
- SQL probes ran as `igaming_runtime`. Go probe tests and 21 mutations ran against per-test scratch databases (`scratchdb`).
- No probe or mutation is committed.

## What holds (verified, not assumed)

| Property | Evidence |
|---|---|
| Actor and scope columns are forced from the session. Client values are ignored. | Q1: a tenant INSERT supplying `changed_by=<platform id>`, `changed_by_scope/engaged_by_scope='platform'`, `version=99` is stored as `tenant` / A1 / `version=1`. MX20 was killed by the implementer. |
| A forged platform GUC is refused | Q2: a tenant staff id placed in `app.platform_admin_principal_id` raises "not a platform-scoped staff principal" |
| No cross-tenant writes | Q3: a T1 session inserting a T2 row is refused by RLS WITH CHECK. Q3b: a T2 staff id under the T1 GUC raises. |
| A player session cannot write and sees zero rows | Q5 |
| Scope is immutable and `version` is forced monotonic | Q6: an update to `provider_scope` raises. `SET version=1` yields `version=2`. |
| No DELETE and no TRUNCATE | Q6: `igaming_runtime` is refused by the grant. The deny trigger is also present. |
| Release without an approved request is refused | Q6 / MX17 |
| A tenant cannot modify, request release of, or approve a platform-engaged row | R1a, R1b, R1d. The tenant lock on the switch row is a third, independent layer (K13 killed). |
| Four-eyes: self-approval is refused | R1e (platform), and the tenant HTTP test. K9 was killed. |
| Four-eyes: approval must be in the release transaction | R1g: approved in tx1, the release in tx2 is refused (MX21) |
| A request is bound to its switch | R4: a request approved for switch X cannot release switch Y. See M6: this is untested. |
| A stale version cannot release | K12 killed |
| KS-L6 for its specified case | R2b: a platform takeover of a tenant-engaged row cancels the open tenant request. K5 killed. |
| Claim predicates work on clean code, all four forms | Probe P2. T1+T2 is refused for a matching `provider/deposit` switch and allowed for another provider. T12 is refused. Cascade T1 is refused under `*`. Player-scoped T1+T2 is refused by RLS (§16.2 item 20). |
| Claim errors never proceed to a call | `drive.go`, `payout.go` and `payout_sweep.go` return on any claim error |
| Audit is in the same transaction as the mutation | `audit.Record` runs inside the `runKillSwitchTx` closure for engage, request, approve and cancel. K17 (drop the approve audit) was killed. |
| Not reachable by players | `RequireStaffPrincipal` and `RequirePermission`. No player, service or support role holds `payments_kill_switch:*`. |

## Ruling on the route deviation (question 2)

**Accepted from a security standpoint, with conditions.** One `/v1/admin/tenants/{tenantID}/payments/...` family with `canActOnTenant` does not weaken any property the ADR's two-tree design was protecting:

- **Platform scope is decided by two independent layers that must agree:**
  - The token's `TenantID == uuid.Nil`, which `staff_users_check` makes possible only for `platform_admin`.
  - The trigger's re-validation that the principal is a `tenant_id IS NULL` staff row.

  A tenant principal cannot produce `'platform'` in any column (Q2).
- **A tenant admin cannot release, downgrade, re-engage or request release of a platform-engaged switch.** The database enforces this in three places (R1a, R1b, R1d). The URL does not matter.
- **Both permission families would map to exactly one role today.** A separate `platform_payments_kill_switch:*` family would add no separation until a second platform role exists.
- **403/404 behaviour is correct:**
  - A tenant principal naming a foreign tenant gets 403, with a denied audit row in its own scope.
  - A foreign object id under the caller's own path gets 404.
  - Player and service tokens get 403.

Conditions:
1. The `architect` records the deviation as an amendment to ADR 0095 §10.4/§10.5. At present only the implementer's code comment and §10.6 note record it. CLAUDE.md requires cross-cutting shape changes to be recorded through the architect. The amendment must restate the invariant that makes it safe: only `platform_admin` may ever hold a nil-tenant token. It must also say that any future platform role granted `payments_kill_switch:*` must be re-reviewed by `security`.
2. The §10.5 "handler verifies that the tenant exists" rule is not implemented (L6).
3. The §10.4 route-table test is missing (L8).

## Findings

### H1 — HIGH (blocks completion): the kill switch's core guarantee has no regression coverage on the main deposit and payout paths

The main paths are:
- T1+T2 (`deposit_v2.go`).
- T1p (`payout.go`).
- T12 (`payout_sweep.go` resubmit).

A mutation that deletes the §10.3 predicate from each of them passes the **entire** `internal/payments` integration suite, the kill-switch HTTP tests and the auth tests:

| Mutation | Result |
|---|---|
| K2: remove the predicate from `InsertSubmittingAttempt` (T1+T2 and T1p, the path every new deposit and payout takes) | SURVIVED |
| K3: remove the predicate from `ResubmitAmbiguous` (T12) | SURVIVED |
| K4: remove the predicate from cascade `InsertCreatedAttempt` | SURVIVED |
| K1: remove the predicate from `ClaimCreatedForSubmission` (T2) | killed. This is the only form under test. |

**Failure scenario:** a refactor of `InsertSubmittingAttempt` drops or mis-binds the `WHERE NOT EXISTS`, for example by passing `$6` for the operation. CI stays green. An operator engages `(provider X, deposit)` during a PSP compromise. The API returns `engaged: true`, but every new deposit still submits to X.

The current code is correct (probe P2), so this is a coverage defect in a safety control, not a live bypass. CLAUDE.md does not allow a security control to be marked done untested. §10.6 also claims the predicate is "exactly as specified" and tested.

The §16.2 item 20 pin ("T1+T2 under a player-scoped context fails and calls nothing") also does not exist as a test.

**Required:** permanent tests, one per claim form. Each needs a positive control:
- An engaged matching switch refuses with `ErrKillSwitchEngaged` or a conflict.
- A non-matching provider or operation succeeds.

The four forms are T1+T2 (deposit), T1p (payout), T12 and cascade T1. Add the player-scoped T1+T2 pin. My probe P2 kills K2, K3 and K4 and can be adapted directly.

### M1 — MEDIUM (before stage gate): the §10.3 L1 "misbound context fails closed" argument is false as written, leaving a latent fail-open

§10.3 says the switch table's tenant SELECT policy "has the same GUC predicate" as `payment_attempts.tenant_staff_scope`. It does not:
- `payment_kill_switches.tenant_scope` also requires `app.platform_admin_principal_id` to be unset.
- `payment_attempts.tenant_staff_scope` (migration 0101) does not.

A transaction with **both** `app.tenant_id` and `app.platform_admin_principal_id` set therefore:
- can claim and write attempts;
- sees **zero** switch rows. The platform policy requires the tenant to be unset.

The predicate then evaluates to "not engaged".

**Reproduced (probe P1):**
- A `'*'/'*'` switch is engaged.
- The control claim under `WithTenant` is refused.
- The same claim after `set_config('app.platform_admin_principal_id', …)` inside that tenant transaction **succeeds**.

No production code sets both GUCs today. I grepped every `set_config`. `kyc/enforcement_admin.go`'s `setActingPrincipal` shows that in-transaction GUC switching is an established pattern. The ADR calls this coupling "binding", yet nothing pins the mixed case.

**Fix, either option:**
- (a) Add `AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL` to `payment_attempts`' write policy.
- (b) Split the switch table's tenant policy into a SELECT policy with exactly the attempts predicate, and keep the stricter INSERT/UPDATE policies. Writes remain blocked by `payment_kill_switch_session()`, which raises on the mixed shape.

Either way, add probe P1 as a permanent test and correct the ADR text.

### M2 — MEDIUM (before stage gate): `provider_scope` is unvalidated, so a typo engages a switch that matches nothing and returns 200

The engage handler accepts any 1 to 64 byte string. The claim predicate is an exact, case-sensitive match. Reproduced at the DB layer: `'Mock-Payments '` is accepted and stored engaged.

**Failure scenario:** during an incident an operator engages `"Mock-Payments"`, or a provider id with trailing whitespace. The API returns `engaged: true, engaged_by_scope: …`, the audit and alert record it, and payments to `mock-payments` continue. This is false assurance from a containment control.

**Required:**
- Reject any `provider_scope` other than `'*'` that is not a provider id registered in the orchestrator registry. Registry membership is code, not tenant-editable, so validation cannot be widened by a tenant.
- Trim and reject whitespace.
- Return 400 on mismatch.

### M3 — MEDIUM (before stage gate): mutation audit rows carry no before/after state

§10.2 and CLAUDE.md require before/after. Every kill-switch audit entry carries only `reason_code`, scopes, `actor_scope` and `target_tenant_id`. None records:
- the prior `engaged` / `engaged_by_scope` / `version` / `reason_code`;
- the resulting `version`;
- whether an engage was a **platform takeover** of a tenant containment (KS-L6);
- which open request KS-L6 cancelled.

The switch row stores only the last actor. The audit log is therefore the **only** history of who contained and released what, and in which order.

**Required:** `before` and `after` objects in `Metadata` for engage, approve+release and cancel. The engage `before` can be read `FOR UPDATE` in the same transaction. Record takeover and KS-L6 cancellations explicitly. Also populate `actor_scope` from the database-forced `changed_by_scope` / `requested_by_scope`, not from the token label (I5).

### M4 — MEDIUM (launch-blocking; test before stage gate): platform interventions are not traceable per tenant

The implementer's `TenantID: uuid.Nil` workaround is **acceptable as an interim measure**. The row is written in the same transaction, attributed to the real platform actor, and cannot be forged by a tenant. It is not sufficient for regulated operation:

- Any tenant-scoped audit read, such as the tenant back office or a B2B own-licence operator's regulator export, **never sees** that the platform engaged, took over, or released that tenant's switch.
- The only path is a platform-scoped query on `metadata->>'target_tenant_id'`:
  - It is untyped JSONB with no index. Only `(tenant_id, created_at)` is indexed.
  - No test pins that the key is present. **K18** (drop `target_tenant_id` from the platform engage audit) SURVIVED.
- A kill switch is exactly the control a regulator asks about ("who stopped payments on this licence, and when").

**Required:**
- Before stage gate: a test asserting that platform-scoped engage, request, approve and cancel audit rows carry `target_tenant_id` equal to the path tenant.
- Before production launch or first B2B tenant, one of:
  - an `audit_log` platform INSERT-only policy family, mirroring 0105's, so a platform action lands under the target tenant's `tenant_id` with actor scope in metadata;
  - or a first-class indexed `target_tenant_id` column.

  This is an `architect` decision because it changes `audit_log`'s RLS, and it should be recorded as such.

### M5 — MEDIUM (launch-blocking, before relaxing the Synthetic tripwire): the §16.2 item 15 reflection test does not scan what production wires, and is evadable

The walker is not vacuous in isolation. Its unit tests prove it flags planted `OutboundCredential`, `Secret`, nested pointer and slice values, and func fields. But:

1. **It scans test-constructed mocks, not the wired registry.** Each domain test builds its own `NewMockProvider(...)` and a skeletal `NewOrchestrator`. It does not scan `cmd/platform-api`'s `paymentsAdapters()`, `casinoAdapters()` or `kycAdapters()`. A real adapter registered in `registrations.go` is **never scanned** unless someone remembers to edit three test files. That is exactly the moment the test exists for.
2. **Evasions confirmed by probe:**

   | Construct | Violations reported |
   |---|---|
   | Credential held in an `atomic.Pointer[OutboundCredential]` | 0 |
   | Credential held in a `chan OutboundCredential` | 0 |
   | Value-typed struct field whose `Authenticator` methods have pointer receivers (`t.Implements` is false for `T`; only `*T` implements) | 0 |
   | Raw `apiKey string` / `[]byte` | 0. This is inherent to a type-based scan. |
   | Direct `OutboundCredential` (control) | 1 |

   `unsafe.Pointer` is also not walked.
3. **The static half is narrower than ADR §11.** It string-matches one file (`mock.go` / `mock_provider.go`) for the `secretstore` import. It does not check:
   - other files in the adapter package;
   - the Fetcher import;
   - package-level variables of credential types.

**Required:**
- Before the stage gate:
  - Correct the §10.6 label for item 15 to **PARTIALLY IMPLEMENTED**.
- Before any non-MOCK adapter is wired:
  - Scan the adapters built by the actual `cmd/platform-api` wiring functions, iterating every registered adapter.
  - Check `reflect.PointerTo(t).Implements(authenticatorType)` for addressable values.
  - Flag `chan`, `unsafe.Pointer` and `atomic.Pointer` / `atomic.Value` holding forbidden types.
  - Make the static check package-wide and add the Fetcher and package-level-var rules.

### M6 — MEDIUM (before stage gate): single-point security checks with no test

Beyond H1, mutations show these checks are the **only** layer for their property and are untested:

| Mutation | Property lost | Result |
|---|---|---|
| K19: `payment_kill_switch_session()` accepts any platform GUC value (drops the `staff_users.tenant_id IS NULL` check) | Any non-platform id in the platform GUC yields `'platform'` actor scope and platform-lock powers. This is the exact S95-C7 / SEC-S91-3 class. | SURVIVED |
| K10: drop `v_req.kill_switch_id <> OLD.id` from the release check | A request approved for switch X releases switch Y of the same tenant (R4 shows the check is what stops it) | SURVIVED |
| K11: drop the `expires_at` check at release | An expired request still releases | SURVIVED |
| K14: drop the 24 h cap on `expires_at` | Requests live indefinitely | SURVIVED |

**Required:** one test each. My Q2 and R4 probes are ready-made.

### L1 — LOW: a tenant session can cancel the platform's release request on a platform-engaged switch

The release-request guard has no scope check on `open→cancelled|expired`. The handler does not check either.

- **Reproduced (R1c):** A1 (tenant) cancels P1's open request on a P1-engaged switch. The request moves to `cancelled`.
- This is the fail-safe direction, because the switch stays engaged. However:
  - It contradicts §10.5 ("refused on platform-engaged rows") and the "tenant is read-only on platform containment" model.
  - It lets a rogue tenant admin keep obstructing the platform's own four-eyes release.

**Fix:** mirror the approve-path check. A non-platform session may not change the status of a request whose switch is platform-engaged, or whose `requested_by_scope = 'platform'`.

### L2 — LOW (DB defence in depth): release requests do not bind to the switch's actual state at INSERT

The HTTP layer covers both gaps below. The trigger does not.

- **Future `expected_version` pre-authorises a later re-engagement (R3).**
  1. A1 files a request with `expected_version = current + 1` for "incident-1 resolved".
  2. B1 re-engages for "incident-2 fraud", which moves the switch to that version.
  3. B1 approves the stale-intent request, and incident-2's containment is released.

  The handler reads the version server-side, so this is not reachable via the API.
- **A request on a non-engaged switch survives a platform engage (R2).** KS-L6 fires only on `true→true` takeovers.
  1. A tenant request filed while the switch was disengaged stays `open` after a platform `false→true` engage.
  2. It then squats the one-open slot. The platform's own request fails with `payment_kill_switch_release_requests_one_open`.
  3. Nothing moves `open` to `expired` on a schedule, so the squat persists until someone cancels it.

  The handler refuses requests on non-engaged switches, so this is DB-only.

**Fix:** in the request INSERT trigger:
- force `expected_version` to the switch's current `version`;
- require `engaged`;
- make KS-L6 fire on any transition that results in `engaged_by_scope = 'platform'`.

### L3 — LOW: `changed_at` is client-controlled

The guard never forces `changed_at`. Q1 stored `changed_at = 2000-01-01` from a tenant INSERT. UPDATEs keep the old value unless the client sets one. This is a forensic column on the switch row that can be backdated or left stale.

**Fix:** `NEW.changed_at := now()` in the guard.

### L4 — LOW: the database accepts a suspended staff principal as actor

`payment_kill_switch_session()` checks existence only. Q4: a `status='suspended'` staff user of T1 was accepted as `changed_by`. JWTs are stateless, so a suspended user's unexpired token can still engage or approve. The DB re-validation is the natural place to add `AND su.status = 'active'`. It is cheap, and it matters most for the approver half of four-eyes.

### L5 — LOW: refusals are neither audited nor diagnosable, and DB failures are reported as 409

`writeKillSwitchError` maps every non-sentinel error to 409 `conflict` and logs `payments_kill_switch_failed` **without the error or a class**. The comment says the text "is logged server-side only for diagnosis". It is not logged.

Consequences:
- A self-approval attempt, or a tenant trying to release a platform containment, leaves no audit row. The transaction rolls back.
- These attempts cannot be told apart from a DB outage.
- An engage that failed on a transient DB error returns "conflict". The engage is idempotent, so an operator can plausibly read this as "already engaged".
- The foreign-tenant denied audit row omits the tenant that was named.

**Fix:**
- Classify trigger refusals (SQLSTATE `P0001` plus a stable prefix) and write a separately committed `OutcomeDenied` audit row naming the switch, request and target tenant.
- Return 5xx for genuine DB errors, as the OpenAPI already documents.
- Log an allow-listed error class.

### L6 — LOW: a platform caller can engage switches for a tenant that does not exist

§10.5 says "the handler verifies that the tenant exists". It does not, and `payment_kill_switches.tenant_id` has no FK to `tenants`. Reproduced: a platform INSERT for `99999999-…` succeeds. The effect is garbage rows plus audit and alert noise. A typo in the tenant id is also silently "contained". Fix with a tenant-existence check returning 404, or an FK.

### L7 — LOW (label): the S95-C7 engage alert is a log line with no delivery and no test

A structured, allow-listed Error-level event emitted after commit is a **reasonable interim**, consistent with the `logSettlementIntegrityAlert` precedent. It is not an alert:
- No alert rule, runbook entry (`docs/runbooks/observability-and-alerting.md`) or metrics backend exists. S9-08 records "no metrics backend is deployed".
- **K21** (delete the `logKillSwitchEngagedAlert` call) SURVIVED. The sportsbook precedent has a test pinning its event; this one does not.

**Required:**
- Before the stage gate:
  - Relabel §10.6 and the task registry: "engage alert event **IMPLEMENTED**; alert delivery **NOT IMPLEMENTED** (depends on the observability stack)".
  - Add a test pinning the event name and fields.
  - Add a runbook entry.
- Recommended: emit an equivalent event on **release**. Lifting a containment is at least as alert-worthy as engaging one.
- Before production launch: real alert routing is launch-blocking under S95-C7.

### L8 — LOW: no §10.4 route-table test

Only `GET …/kill-switches` is tested with a player token. **K15** (remove `RequireStaffPrincipal` from all 7 routes) SURVIVED; `RequirePermission` still denies, because no player role holds the permissions. Add the route-table test §10.4 requires:
- enumerate all 7 kill-switch routes;
- assert 403 for player and service principals;
- assert the routes are absent from every player or public mux.

### Informational

- **I1 — GUC trust is the boundary.** The database believes whatever principal the application role puts in the GUCs.
  - R1h: one transaction that sets P1, files a request, switches to P2, approves and releases succeeds.
  - Four-eyes is therefore as strong as the app's token authentication and the absence of SQL injection. This is the same trust model as migrations 0044 and 0093, and is accepted.
  - The database does not verify that the approver holds `payments_kill_switch:release`.
- **I2 — Policy shape.** Both 0105 policy families are `FOR ALL`, while §10.2.1 says "no FOR ALL". DELETE is blocked by the grant and the trigger, so the effect is equivalent. Align either the ADR text or the policies.
- **I3 — In-flight window.** A claim whose statement snapshot predates the engage commit still proceeds, and its call happens after the operator sees 200. The window is one in-flight claim per concurrent worker or request, not "one call" in total. This is accepted by §10.3. Runbooks should tell operators to list `submitting` attempts after engaging.
- **I4 — `validateManifest`.** It is correct for its two fields. It runs at `WriteCapability`, not when the adapter is added to the registry or at new activity, so it is not yet "refuses to start" (§10.1). The Synthetic exemption is a structural marker method, so an adapter that embeds a MOCK type would inherit it. The remaining fields are deferred (PRH-I1-MANIFEST-*). This is `ledger-finance`/`payments` territory.
- **I5 — Actor scope label.** `actor_scope` in audit metadata is derived from the token, not from the database-forced scope. The two agree today (see M3).
- **I6 — Redundant platform-lock layers.** K6 (release-trigger platform-pair check), K7 (approve-scope check) and their combination K20 each SURVIVED. The tenant lock on the switch row (K13, killed) still blocks the release. Nothing tests the approval layer on its own; a test for "tenant approves a platform request → refused" is recommended.

## Fail-safe (question 4) summary

- **Predicates and race:**
  - The claim predicates are inside the claim statements and correct on clean code (P2).
  - There is no time-of-check/time-of-use gap beyond the statement-snapshot window (I3).
- **DB errors:** a DB error aborts the claim and no call is made.
- **Latent bypass:** the mixed-GUC context (M1).
- **Coverage:** none of this is regression-tested outside T2 (H1).
- **Callbacks and polls (§10.3):** broken today. Ledger-finance H3 is in progress. The same `InsertCreatedAttempt` refusal is reached from **three** sites, and the fix must cover all of them:
  1. `receipt.go` (callback decline cell): reported as H3.
  2. `sweeper.go` `QueryStatus` definite-decline arm. A **poll** result is rolled back and re-polled indefinitely while `*` is engaged, which contradicts §10.3's "polls never stopped".
  3. `drive.go` phase-C application of a synchronous decline to a call that **was already sent**. The provider's answer is rolled back.

## Launch-blocking summary

These must be resolved before production launch authorization:
- H1;
- ledger-finance H3 at all three sites;
- M1, M2, M4 and M5;
- real S95-C7 alert delivery (L7).

M3 and M6 are required before the stage gate. The route deviation is accepted, subject to the architect recording it.

## Evidence index (not committed)

- **SQL probes as `igaming_runtime`:** Q1 to Q6 and R1 to R5.
- **Go probes:**
  - `TestSECKS_P1_MixedGUCContextClaimsPastEngagedSwitch` (fail-open confirmed);
  - `TestSECKS_P2_MainPathPredicates` (clean code correct; kills K2, K3 and K4);
  - `TestSECKS_Evasion` (credentialscan).
- **Mutation results:**

  | Result | Mutations |
  |---|---|
  | Killed | K1, K5, K8, K9, K12, K13, K16, K17 |
  | Survived | K2, K3, K4, K6, K7, K10, K11, K14, K15, K18, K19, K20, K21 |

  Baseline green: the full `internal/payments` integration suite, the kill-switch HTTP and OpenAPI tests, and the auth permission tests.
