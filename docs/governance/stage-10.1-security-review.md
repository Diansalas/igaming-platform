# Stage 10.1 — post-implementation security review (ADR 0090)

- **Reviewer:** `security` (mandatory review; CLAUDE.md "Security" and ADR 0090 item 3).
- **Diff reviewed:** `git diff 8561ac2..250828b`. It contains `e9e0ad8` (PAY-REV-1, SB-T1-XMIN) and `250828b` (PAY-WH-TENANT-1).
- **Inputs:**
  - CLAUDE.md and ADR 0090.
  - `docs/plans/stage-10.1-planning-gate-proposal.md` §E, §K and §M.
  - `docs/plans/stage-10.1-planning/04-review-security.md` (S-1..S-7, X-1..X-3).
  - `11-pay-wh-tenant-1-design.md` with its binding rulings.
  - `16-pay-wh-review-qa-test-plan.md`.
- **Date:** 2026-09-26.

## Verdict

**APPROVE WITH REQUIRED CHANGES.** No P0 or P1 findings.

| Workstream | Security position |
|---|---|
| SB-T1-XMIN (0093) | **Cleared.** The anchoring deviation is accepted, and it fails closed (see §3). Only P3 notes remain. |
| PAY-REV-1 (0092, L2 lock, 409) | **Cleared for the financial control.** P2-2 (denial audit content) must be fixed before the item is labelled `IMPLEMENTED`. |
| PAY-WH-TENANT-1 | **Not yet cleared.** P2-1 (a pre-verification response oracle that contradicts the uniform-401 contract) and P2-3 (the binding security tests T11a, T12 and T13 are missing) must be fixed first. The cryptographic tenant binding itself is correct. The cross-tenant write effect of S-6 is closed **for the MOCK**. |

**Launch position (unchanged, restated so no record overstates it):**
- S-6 / PAY-WH-TENANT-1 is **closed for the MOCK adapter only**.
- It is still **launch-blocking for any real PSP**. The real `WebhookCredentialResolver` (handle table plus secret store) is `NOT IMPLEMENTED`, and PAYWH-TS-1 (signed-timestamp replay window) is not built.
- **KYC-WH-1 (High)** remains open and launch-blocking. Its committed constant secret enables KYC self-approval on any deployment of this binary, staging included. This diff does not change it.
- CAS-WH-TENANT-1 (Medium) and PAYWH-RL-1 remain open.

## Tests run (local CI Postgres, NOBYPASSRLS roles)

- **Targeted run:** `go test -tags integration -run 'PayRev1|Migration0092|Migration0093|TenantBinding|Webhook|PayWH|ComposedVoid|Mock|Conformance|LockOrder|Capability'` on `payments`, `ledger`, `sportsbook` and `httpserver`. All pass.
- **Full integration packages:** `httpserver`, `ledger`, `sportsbook` and `db` pass in the working tree. `payments` passes in a clean worktree of `250828b`.
  - In the shared working tree, `payments` currently **fails to build**. The cause is two *untracked* files that are not part of the reviewed commit and were presumably added by another agent while this review ran: `internal/payments/webhook_no_write_before_verification_integration_test.go` and `webhook_replay_duplicate_integration_test.go`. The build error is `webhook_replay_duplicate_integration_test.go:27: undefined: uuid`. These files were not reviewed.
- **Unit tests:** `go test ./internal/payments/ ./internal/httpserver/` passes. `go vet` on the four packages is clean.
- **Temporary probe:** a throwaway HTTP test, deleted immediately afterwards, confirmed P2-1 empirically. Results are quoted in P2-1.

## Verification of required changes

### PAY-REV-1

| Req | Status | Evidence |
|---|---|---|
| S-1 tenant-leading partial unique index | **Met** | `migrations/0092_…up.sql:55-57`: `(tenant_id, reverses_transaction_id) WHERE transaction_type='deposit_reversal'`. The non-tenant-composite FK is recorded as deferred. |
| S-2 RLS-proof refusal | **Met** | `0092…up.sql:54-61`: a `DO … EXCEPTION WHEN unique_violation` around `CREATE UNIQUE INDEX`. There is no `SELECT` pre-check, no `SET row_security` and no `CONCURRENTLY`. The refusal text contains no ids, amounts or references, and the original `DETAIL` (which would echo the key) is replaced. `internal/ledger/migration_0092_integration_test.go:150-205` seeds duplicates in **two tenants** and runs as the scratch-database owner. `scratchdb.assertUnprivilegedOwner` asserts that owner is `rolsuper=false, rolbypassrls=false`. The test also checks that no partial index is left behind and no rows are deleted. |
| S-3 denial audit in a separate committed tx | **Mechanism met; content not met (P2-2)** | `deposit_handlers.go` (ErrDepositAlreadyReversed branch) opens a fresh `WithTenant` after the rollback. An audit failure is logged and still returns 409. `payrev1_webhook_integration_test.go:112-127` reads the row from a fresh tx after the response. The audit target and metadata are wrong (P2-2). |
| S-4a tenant predicate on the lock | **Met** | `orchestrator.go:1055` `… WHERE id = $1 AND tenant_id = $2 FOR UPDATE`. |
| S-4b missing row / wrong type fails closed, never tombstones | **Met** | `orchestrator.go:1058-1068` returns `ErrDepositReversalIntegrity`. The tombstone branch (`:1010-1027`) is decided before the lock, only on `!found \|\| LedgerTransactionID == nil`. |
| S-4c lock only after signature verification | **Met** | The lock is reached only through `ReceiveCallback` → `HandleCallback` success (`orchestrator.go:897-925`). This was re-verified against the merged PAY-WH-TENANT-1 code (ruling 4). |
| S-4d note that `FOR UPDATE` needs UPDATE privilege | **Not recorded (P3-4)** | ADR 0082 A5 does not mention it. |
| S-5 409 with generic body; alert allow-list | **Met** | `payment_callback_errors.go:66-71` returns 409 "callback rejected". The alert `payment_webhook_integrity_alert_deposit_already_reversed` logs only `provider_id`, `tenant_id` and `request_id`, with no `err`. The ledger backstop error (`ledger.go`, `ErrReversalAlreadyExists` with the tx id) is never logged by the handler. The optional part of S-5 was partly taken: `ErrCallbackProviderMismatch` no longer logs amounts, but it still returns 500 (P3-5). |
| Ledger constraint routing | **Met** | `db.IdempotentInsert` returns `pgErr.ConstraintName`. `ledger.Post` routes `ledger_transactions_one_deposit_reversal` to `ErrReversalAlreadyExists` **before** the idempotency-key lookup. Payments maps it to `ErrDepositAlreadyReversed`. Other callers ignore the new return value, so their behaviour is unchanged. |
| Tenant isolation | **Met** | `payrev1_tenant_isolation_integration_test.go` covers it. The lock runs under the tenant GUC plus an explicit predicate. |

### SB-T1-XMIN

| Req | Status | Evidence |
|---|---|---|
| X-2 NULL ⇒ reject | **Met (code)** | `0093…up.sql:285-288`: `pg_xact_status(...) IS DISTINCT FROM 'in progress'` ⇒ RAISE. NULL rejects. |
| X-2 error ⇒ reject | **Met** | `:280-292`: the whole reconstruction is inside `BEGIN … EXCEPTION WHEN OTHERS THEN RAISE <T-1 message>`. |
| X-2 epoch handling | **Deviation accepted**, see §3 | |
| X-2 NULL/error tests | **Partially met (P3-3)** | The error case is tested. The "NULL" case does not actually reach the NULL path. |
| X-3 SECURITY INVOKER, no search_path, no REVOKE, other branches byte-identical, down verbatim | **Met** | There is no `SECURITY DEFINER` (the only match is a comment) and no `SET search_path`, matching 0091. No migration REVOKEs `pg_xact_status`. `TestMigration0093_UpChangesOnlyFunctionBody` and `TestMigration0093_DownRestoresExactPriorFunctionBody` pass. |
| X-3 rolled-back savepoint rejected | **Met** | A row rolled back to a savepoint is invisible, so it hits `cause.id IS NULL`. |

### PAY-WH-TENANT-1

| Requirement | Status | Evidence |
|---|---|---|
| Tenant bound into verified material | **Met** | `mock.go` `signingInput`: `"igaming.payments.webhook.v1"\0tenant_id\0provider_id\0key_id\0<raw body>`. The verifier rebuilds it from the **route-resolved** `tenantID`. `ReceiveCallback` overwrites `in.TenantID/ProviderID` (`orchestrator.go:857-858`). T5 (shared secret, A→B ⇒ 401) passes. |
| Per-(tenant, provider) credential; single candidate; no cross-tenant trial | **Met (MOCK)** | `MockWebhookCredentials.Resolve` derives `HMAC(master, label\0tenant\0provider)` and resolves only `mock-v1` for its own provider. The orchestrator re-checks `cred.TenantID/ProviderID` (`:884-889`). A nil resolver fails closed (T14). |
| No secret in repo | **Met** | `masterSecret` is 32 bytes of `crypto/rand` per process, and generation panics on RNG failure. There is no config or env var. The only literal is the test-only T5 fixture `"this-is-not-a-real-secret-only-32b"` (`webhook_tenant_binding_integration_test.go:208`), which is acceptable. The diff contains no other key-like literal. |
| Constant-time comparison | **Met** | `hmac.Equal` over two 64-char lowercase hex strings. The header format is regex-validated first, so lengths are fixed. |
| Header validation | **Met, but ordered late (see P2-1)** | `ParseWebhookAuthHeaders`: `^v1=[0-9a-f]{64}$` and `^[a-z0-9-]{1,32}$`. |
| Key-material scan before verification + alert (C5) | **Met** | `mock.go:469-476` scans first. `ReasonKeyMaterial` feeds the allow-listed warn line. |
| Uniform 401 for pre-verification failures | **Not met (P2-1)** | Two request shapes still differ. |
| Log allow-list | **Met (code); untested (P2-3)** | `callbackAuthFailureAllowlistFields` logs only request_id, reason, tenant_id if resolved, provider_id if valid, key_id if valid, the fingerprint only for signature_invalid, client_ip and body_len. There is no `err`, body, header value or slug. |
| No audit row for unauthenticated failures | **Met (code)** | No `audit.Record` runs before verification. The T2/T3 fresh-tx assertions show zero `audit_log` delta. |
| I1: pre-verification path has no tenant-scoped financial read/write | **Met (by inspection)** | Before `HandleCallback` succeeds, the only tenant-scoped statement is `ProviderAcceptsWebhook`'s read-only `EXISTS` on `provider_capabilities`: explicit tenant predicate, RLS-scoped, no `FOR UPDATE` (`capability.go`). The only other pre-verification DB access is the platform `GetTenantBySlug`. `WithTenant` itself only sets the GUC. The deposit_intents lookup, L2 lock, tombstone, ledger and audit all come after step (e). T11a, the required statement-capture test, is missing (P2-3). |
| I3: one tenant id for RLS GUC, MAC input and credential | **Met** | `t.ID` is used for `WithTenant`, `ReceiveCallback(tenantID)`, `Resolve` and `signingInput`. |
| RLS on `ProviderAcceptsWebhook` | **Met** | `TestProviderAcceptsWebhook_RLSScoped` (T11b). |
| Simulate route: tenant from JWT only; bytes never leave the process | **Met (code); T13 absent (P2-3)** | `payment_deposit_simulation_handlers.go` signs for `tc.TenantID`. The response is status only. An auth failure maps to 503 there. |
| Secret redaction | **Mostly met (P3-1)** | `String()` and `LogValue()` redact. `GoString` is missing. |
| OpenAPI (API-DOC-PAYWH) | **Met, with the inaccuracies noted in P2-1 and P3-5** | `platform-api.yaml:2455-2560`. The contract test passes. |
| KYC and casino code unchanged | **Met** | Nothing under `internal/kyc/` or `internal/casino/` changed. The only casino-named file touched is `stage7_b2c_casino_acceptance_test.go`, and only its payments `CallbackPayload` call site. |

## Findings

### P2-1 (Medium, REQUIRED before PAY-WH-TENANT-1 is labelled IMPLEMENTED): pre-verification response oracle survives the uniform-401 contract

**Where:**
- `internal/payments/mock.go:471-472`: the pre-verification `json.Unmarshal` failure returns a plain wrapped error, not an auth failure.
- `internal/payments/orchestrator.go:915`: that error is wrapped generically.
- `internal/httpserver/deposit_handlers.go:404`: the generic branch returns 500 and runs `logger.Error(..., "error", err)`.
- `deposit_handlers.go:287-316`: the tenant lookup runs **before** the body-size and header checks, which contradicts ruling 5 ("header format validation … before any tenant or database work").
- `docs/api/openapi/platform-api.yaml:2480` and `:2526` claim that every pre-verification failure gets the IDENTICAL 401.

**Observed, unauthenticated, with well-formed but invalid signature headers (temporary probe, since deleted):**

| Request | Response |
|---|---|
| active tenant + configured provider + body `not json` | **500** `internal_error "failed to process callback"`, plus an ERROR log `payment_webhook_failed error="…parse callback: invalid character 'o' …"` |
| provider not configured, same body | 401 `callback rejected` |
| unknown slug, same body | 401 `callback rejected` |
| active tenant, body > 1 MiB | **400** `validation_error "request body too large"` |
| unknown slug, body > 1 MiB | 401 `callback rejected` |

**Failure scenario:**
1. An unauthenticated attacker sends one non-JSON POST per candidate slug and provider.
2. A 500 tells them the tenant is active **and** has that provider configured for webhooks. A 400 on an oversized body tells them the tenant is active.
3. This is exactly the tenant/provider enumeration the design's §3.2 and T9 set out to remove. The OpenAPI entry wrongly promises it cannot happen.
4. In addition, anyone can make the platform emit ERROR-level `payment_webhook_failed` lines that carry body-derived bytes. That enables alert fatigue and log poisoning, against the §3.3 principle that unauthenticated input must not reach error or alert channels except the allow-listed auth line.
5. There is no financial effect and no cross-tenant write, so this is Medium, not High.

**Required fix:**
1. **Parse failures.** Any `HandleCallback` failure that happens before HMAC verification succeeds must surface as a `*CallbackAuthError`. Either add a closed reason such as `body_malformed`, or reuse `signature_invalid`. Options:
   - have the adapter return a sentinel (for example `ErrCallbackUnverifiedMalformed`) for pre-verification parse errors, and map it in `ReceiveCallback` next to `ErrInboundKeyMaterial` and `ErrCallbackSignatureInvalid`;
   - or make the adapter contract explicit: every error before verification is one of those auth sentinels.

   Parse errors *after* verification may keep a distinct code.
2. **Ordering.** Move the body read, the 1 MiB limit and `ParseWebhookAuthHeaders` **before** `GetTenantBySlug` in `deposit_handlers.go`, as ruling 5 requires. The oversized-body 400 is then tenant-independent. Alternatively, map oversized bodies to the uniform 401.
3. **T9 coverage.** Extend `TestWebhook_EnumerationOracle_IndistinguishableResponses` with a non-JSON body probe and an oversized-body probe against the active configured tenant, both expecting the byte-identical 401.
4. **OpenAPI.** Correct the 400 description if the ordering changes.

### P2-2 (Medium, REQUIRED before PAY-REV-1 is labelled IMPLEMENTED): denial audit lacks the entity and the rejected reference, so the rejected reversal is recorded nowhere

**Where:** `internal/payments/orchestrator.go:1194-1203` (`RecordDepositReversalRejection`). It writes `TargetType: "payment_webhook", TargetID: providerID`, and its metadata is only `{provider_id}`.

**What was required:**
- S-3 required target = the original **deposit_intent** and metadata `{provider_id, rejected reversal_provider_reference}`.
- QA plan test 3 requires that tenant, actor and entity are populated.
- CLAUDE.md requires that audit records name the entity.

**Failure scenario:**
1. A PSP sends a genuine second reversal for one deposit, for example a refund followed by a chargeback. The platform correctly refuses to post it (409).
2. The alert line intentionally carries no reference (S-5).
3. The failed transaction was rolled back.
4. The only durable record is an audit row that says "some reversal via `mock-payments` was rejected". It names no deposit, no intent and no PSP reference.
5. Operations and PSP reconciliation, which ADR 0090 says "must see it", cannot identify which deposit or which PSP event to reconcile without the PSP's own logs. The only link is a request_id join to a log line that holds no reference either.
6. Contrast the tombstone audit (`orchestrator.go:1014-1021`), which records both references.

The rejected reference has been authenticated by then (the signature was verified), so storing it does not let unauthenticated input write to the store.

**Required fix:**
1. Return a typed error, or extend `ErrDepositAlreadyReversed` into a struct error, carrying the verified `original.ID` (deposit_intent id) and `event.ProviderReference`.
2. Have the handler pass both to `RecordDepositReversalRejection`.
3. Record `TargetType: "deposit_intent", TargetID: <intent id>` and metadata `{provider_id, reversal_provider_reference}`.
4. Keep the alert log unchanged: no reference goes in the log.
5. Extend `payrev1_webhook_integration_test.go:112-127` to assert the target and the reference, still from a fresh tx.

### P2-3 (Medium, REQUIRED before PAY-WH-TENANT-1 is labelled IMPLEMENTED): binding security tests are missing at the reviewed commit

At `250828b` the following tests from the binding QA plan (paper 16), which are the security-specified tests for I1 and the allow-lists, do not exist:

- **T11a** `TestWebhook_BadSignature_NoWriteBeforeVerification`: a statement-capturing tx showing zero INSERT, UPDATE or `FOR UPDATE` statements before verification. This is the only automated proof of invariant I1, which today rests on code inspection alone. An untracked file with this name appeared in the working tree during the review. It is not committed and was not reviewed.
- **T12** `TestWebhook_AuthFailureLogging_AllowListOnly`: for each reason, captured log keys must be a subset of the allow-list, with no body, signature, header value, raw slug or `err`, and zero `audit_log` rows for the 401. Nothing pins the allow-list, so a future edit that adds `"error", err` to the auth-failure path would go unnoticed.
- **PAY-REV-1 alert allow-list:** no test asserts that `payment_webhook_integrity_alert_deposit_already_reversed` carries only provider_id, tenant_id and request_id. The sportsbook precedent is `sportsbook_settlement_alert_audit_test.go:128-160`.
- **T13 / `TestSimulationRoute_CannotNameOtherTenant`:** the header of `payment_webhook_tenant_binding_test.go:5` claims T13, but no such test exists. It should prove that the simulate route signs for `tc.TenantID` only, and that a tenant field in the request body is ignored.

(T8a/T8b replay tests appear to be in progress in another untracked file. That file currently does not compile.)

**Required fix:** commit these tests, run them against the NOBYPASSRLS runtime role, and re-run this review's test step.

### P3-1 (Low): `WebhookCredential` has no `GoString`, so `%#v` prints the secret

`internal/payments/types.go:260-273` implements `String()` and `LogValue()`, but `fmt.Sprintf("%#v", cred)` bypasses `Stringer` and prints `Secret:[]byte{…}`. This is the same for any struct that embeds the credential. **Fix:** add `func (c WebhookCredential) GoString() string { return c.String() }`, and add a `%#v` case to `TestMockWebhookCredentials_KeyDerivation`. Today the only credential secret is a per-process mock secret, but the real resolver will reuse this type.

### P3-2 (Low / informational): SB-T1-XMIN anchoring deviation accepted; one theoretical residual remains

**The deviation.** 0093 anchors the epoch to `pg_current_xact_id()` rather than `pg_snapshot_xmax(...)` (ruling R-2). It accepts only if `xact_full >= pg_current_xact_id()` **and** `pg_xact_status(xact_full) IS NOT DISTINCT FROM 'in progress'`, with NULL or any error ⇒ reject. I agree that the `pg_snapshot_xmax` construction reconstructs a still-open or released savepoint's xmin one epoch low, which reproduces the defect. The deviation is the correct choice.

**How each case fails closed:**
- A row from the checking transaction's own tree: subxids are assigned after the parent's xid, so the row reconstructs ≥ the top-level xid and reports `in progress`. It is accepted, as intended.
- An earlier committed row in the same epoch reconstructs below the top-level xid ⇒ reject.
- A later committed row, visible under READ COMMITTED, reports `committed` ⇒ reject.
- A concurrent uncommitted row is invisible ⇒ `cause.id IS NULL` ⇒ reject.
- A row whose reconstruction lands in the future makes `pg_xact_status` raise ⇒ caught ⇒ reject.
- A clog-truncated row returns NULL ⇒ reject.
- The top-level transaction wrapping into a new epoch mid-transaction reconstructs low ⇒ false reject (fail closed).

**The one residual accept path.** A **visible** rollback row for the *same bet* that is ≥ 2^32 transactions old, whose xmin low 32 bits land exactly on the xid of a *different, currently in-progress* transaction assigned after ours. That other transaction reports `in progress` ⇒ accept. The same aliasing class already existed in 0091, where a row's low bits equal to our own xid was accepted, so this is not a regression.

**Impact:** only T-1's causation provenance (a composed void citing an ancient rollback). Every other void rule still applies: no un-reversed settlement, not already void, and the ledger type and correlation must match. **No change is required.**

Optional hardening: add an independent same-transaction check, for example `cause.created_at = transaction_timestamp()`, if `created_at` is server-defaulted and not client-settable. Also record in ADR 0088 §3.3 that security accepted this deviation. The migration header asks for architect and ledger-finance re-review, and that is still their call.

### P3-3 (Low): the X-2 "NULL" test does not exercise the NULL path, and it tests a replica

`internal/sportsbook/settlement_migration_0093_integration_test.go:395-418` passes `raw=3`. That reconstructs to `epoch|3 < ref`, so the `< ref` clause rejects it and `pg_xact_status` never sees a NULL. The probe function (`:352-366`) is also a hand copy, not the trigger, so the two can drift. The code's predicate is visibly correct (`IS DISTINCT FROM`), which is why this is P3.

**Fix (any one):**
- Add a probe case that forces the status to NULL, for example by wrapping the status call so a test can substitute NULL, and asserting FALSE.
- At minimum, correct the test comment so it does not claim NULL coverage.
- Add an assertion that extracts the guard expression text from `pg_get_functiondef` and compares it to the probe's, so drift fails the test.

### P3-4 (Low): S-4d not recorded

ADR 0082 A5 does not note that the new `FOR UPDATE` on `ledger_transactions` requires the runtime role to keep UPDATE privilege on that table. If UPDATE is ever REVOKEd, as it was for `sportsbook_bet_settlements`, this lock and the casino rollback lock fail closed, with every reversal returning 500. **Fix:** add one sentence to A5.

### P3-5 (Low): remaining status-code inaccuracies after verification

- `payment_callback_errors.go:72-77`: `ErrCallbackProviderMismatch` on the public webhook still returns **500**. The amounts are no longer logged, which is good. A real PSP retries a 500 indefinitely. S-5 recommended 409. This is optional, but it should now be decided explicitly.
- OpenAPI `platform-api.yaml:2526` documents 400 for a "malformed body after signature verification". Verified-but-malformed bodies (missing `provider_reference`, unsupported `event_type`, invalid outcome) actually return 500. A verified body that still carries a legacy `signature` field returns 401. Align either the code or the documentation.

## Scope of this review

**Covered:**
- The full diff `8561ac2..250828b`: Go code, migrations 0092 and 0093 (up and down), OpenAPI, and the ADR/architecture doc changes insofar as they make security claims.
- The payments webhook and simulate-route handlers, `ReceiveCallback`, mock signing and resolution.
- `ProviderAcceptsWebhook` RLS, the PAY-REV-1 lock and backstop, the ledger constraint routing and `IdempotentInsert`.
- The T-1 trigger change.
- A secret scan of the diff.
- Confirmation that the KYC and casino code is unchanged.

**Not covered:**
- Real PSP adapters (none exist) and the real resolver or secret store (`NOT IMPLEMENTED`).
- Volumetric DoS and rate limiting (PAYWH-RL-1).
- Timing side channels. The DB round trip versus the early 401 is the accepted residual from design §3.2.
- The two untracked test files that appeared during the review.
- KYC-WH-1 and CAS-WH-TENANT-1 fixes (out of scope; still open).
- Penetration testing. This is a code- and design-level review, not an external audit.

Passing this review does not make the webhook "secure" for production. For any real PSP, the launch blockers listed under **Verdict** still stand.
