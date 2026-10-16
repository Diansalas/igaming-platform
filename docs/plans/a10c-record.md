# A10c record: console backend gaps (from A10b) and the b2c OpenAPI completion

Branch `a10c-console-backend`, base `6bf9642` (integration tip `origin/claude/focused-wright-jw88w9`: A5, A6, A8, A11, A12 merged).
Providers stay MOCK. Nothing here is a legal or regulatory claim. ADR 0119 S8 lifecycle UI is class B and was NOT built.
Registry, HANDOVER, progress, active-stage and the decision register were not edited (the orchestrator is the single writer).

## Delivered (labels per CLAUDE.md)

| # | Deliverable | Label | Where |
|---|---|---|---|
| 1 | Reconciliation single-run read `GET /v1/admin/tenants/{tenantID}/reconciliation/runs/{runID}?stream=` | IMPLEMENTED (MOCK stack) | `staff_reconciliation_routes.go`, `reconciliation/read.go` (`GetRunForStream`) |
| 2 | Reconciliation single-mismatch read `.../reconciliation/mismatches/{mismatchID}?stream=` and the `run_id` filter on the mismatches list | IMPLEMENTED (MOCK stack) | same; `StaffMismatchFilter.RunID`, `GetMismatchForStream` |
| 3 | Payment-attempt event history `GET .../payment-attempts/{attemptID}/events` (verified provider receipts, closed vocabularies, paged) | IMPLEMENTED (MOCK stack) | `payment_attempt_events_routes.go` |
| 4 | Hold-candidate list `GET .../withdrawal-hold-candidates` (R3) | IMPLEMENTED (MOCK stack); production use still gated by Q-HSEC-1/2/3 (ADR 0111) | `withdrawal_hold_candidates_routes.go` |
| 5 | Attempt list filters `provider_id` (exact) and `min_age_seconds` | IMPLEMENTED | `payment_attempts_read_routes.go` |
| 6 | Paged K1 lists (optional `?limit` / `?offset`, `X-Total-Count`, bare array kept) | IMPLEMENTED | `capability_routes.go`, `capability/capability.go` (`List*Page`) |
| 7 | Server-side `Idempotency-Key` on the 13 K1 / K3 / HSEC mutating routes (optional header; migration 0164 `staff_idempotency_keys`) | IMPLEMENTED (optional, see residual R2) | `staff_idempotency.go`, `migrations/0164_*` |
| 8 | `evidence_ref_hash` decision | NOT IMPLEMENTED / BLOCKED on the owner decision (ADR 0114 9.4, ADR 0111 25.7); options below | none |
| 9 | Backoffice wiring: parked-payments provider/age filters and event timeline; reconciliation single reads and the "only this run" filter; hold-candidate picker; K1 pager; idempotency doc | IMPLEMENTED against the new routes (MOCK banner unchanged) | `backoffice/src/{api,features}` |
| 10 | b2c OpenAPI completion (7 routes / bodies that were missing) | IMPLEMENTED | `docs/api/openapi/platform-api.yaml` |
| 11 | b2c contract test that pins every b2c api path/method/body/query to the spec (generalises the A6 `rg.contract.test.ts`, no duplicate) | IMPLEMENTED | `b2c/src/api/openapiContract.test.ts`, `b2c/src/test/openapiContract.ts` |
| 12 | ADR 0119 S8 lifecycle UI | NOT IMPLEMENTED (class B, deferred as instructed) | none |

### Route freeze (all four new routes are reads, step-up `none`)

- `staffRoute(deps, stepUpNone, ...)` for all four; golden table +4 entries; pin 218 -> **222 routes, 135 mutating (unchanged)**.
- OpenAPI: 245 paths now (was 237: +4 staff reads, +4 player reads); each new operation documents 400/401/403/404/500.
- Permissions are existing ones: events = `payment_force_resolve:read` alone (like R1); run/mismatch single reads = the same per-stream permission
  as the list, checked BEFORE any query; candidates = `withdrawal_hold_resolution:read` (platform_admin only) AND a platform token.
- Tenant scope: path tenant through `canActOnTenant` (tenant caller naming another tenant = 404, no data), RLS read-only transaction on the path
  tenant plus an explicit `tenant_id` predicate, platform reads write the tenant-visible platform-action audit row first (fail closed; unknown
  tenant = 404 and no audit row).
- Data shown: nothing the consoles did not already show. Events: ids, closed vocabularies, amount/asset, timestamps; never provider, original,
  merchant or settlement reference, the provider's decline text, or the fingerprint. Candidates: withdrawal id, brand id, amount, asset, exponent,
  last-change time, tenant and brand status; never a player, wallet or destination id.

### Design decisions taken inside the approved architecture

- **No new SECURITY DEFINER function or RLS policy for R3.** ADR 0114 R3 sketched "ONE narrow SECURITY DEFINER function". It is not needed:
  `withdrawal_requests` already has a tenant-scoped policy, `tenants` / `brands` are readable, and the read is a single-tenant RLS read like R1.
  Not adding a definer function is the smaller privilege surface. Tell the architect if a definer function is still wanted.
- **`open_resolution_id` is not returned on candidates** (hold resolutions are readable only in an acting session, ADR 0114 12.4); the console
  lists the existing requests next to the candidates and says so.
- **Idempotency design (the one new mechanism).** A tenant-owned table (`tenant_id`, FORCE RLS on `app.tenant_id`, staff sessions only), unique
  on (tenant, acting staff id, key). The wrapper sits INSIDE `staffRoute` and the route's permission gate, so authentication, aal2, liveness,
  step-up and permissions have all passed before a key is looked at. It reserves the key (own tx), runs the handler into a buffer, stores a 2xx
  response (<= 256 KiB) and replays it with `Idempotent-Replayed: true`; same key + different method/path/query/body = 409
  `IDEMPOTENCY_MISMATCH`; same key while running = 409 `IDEMPOTENCY_IN_PROGRESS`; a non-2xx outcome or a panic deletes the reservation. It
  decides nothing: the four-eyes rules, proofs, state machines and counts stay the flows' own database guards. Two codes were added to
  `apierror` and the OpenAPI `Error.code` enum.
- **K1 lists keep the bare array** (changing to the `{items, total}` envelope would break every existing client and test); paging is optional
  and the total is the `X-Total-Count` header. The UI asks for one row more than it shows to know whether a next page exists.
- **`provider_id` filter** reuses `webhookauth.ValidProviderID` (`^[a-z0-9][a-z0-9-]{0,62}$`); `min_age_seconds` is 0..31536000.

### evidence_ref_hash (deliverable 8): options only, no decision taken

It is an OPEN owner decision, not an engineering one (ADR 0114 9.4 / 0111 25.7), so nothing was built. Facts for the owner:
- (a) **platform digest** over `(provider_id, R, amount, asset, merchant_reference)` shown to approvers (ledger-finance's view): approvers compare
  a value the platform computed; R never leaves the server; needs a KEYED hash (HMAC with a platform or per-tenant secret, gating item G-S11-2)
  because a bare SHA-256 of a short PSP reference is brute-forceable.
- (b) **hash of the portal confirmation with blind entry** (security's view): each approver types R independently and the server compares; needs
  the blind-entry screens (S11) and `provider_reference_at_submission` redaction first (G-S11-1).
- RECOMMENDATION (not a Blueprint requirement): keep today's behaviour (the operator supplies a 64-hex digest computed outside the console; the
  console never sees R) until the owner picks (a) or (b); both stay MOCK-only until D-7, T10, S-3.

## Residuals (honest list)

- **R1** Migration number `0164` may collide with a sibling branch (0159 and 0161 are absent from the base). The orchestrator renumbers at merge.
- **R2** The `Idempotency-Key` header is OPTIONAL on these routes (the consoles always send it). Making it required would change every existing
  Go test and any other client; not done. Without the header behaviour is unchanged (idempotent by state).
- **R3** `staff_idempotency_keys` has no retention job (rows are tiny but unbounded) and a crash between the handler's commit and the completion
  write leaves a key `in_progress` (retries get 409 `IDEMPOTENCY_IN_PROGRESS`; a new key is safe because the flow guards refuse a double act).
- **R4** Still open from the A10b list: G4 (platform `open_resolution_id` is always null), G6 (K1 filters, platform staff in the directory, no K1
  payload hash), G7 closed earlier by A10b fix round, G8 (above).
- **R5** `provider_id` / `min_age_seconds` are filters on the keyset list; the console age options are 1h, 6h, 1d, 3d, 7d.
- **R6** The b2c contract checker is a COPY of the backoffice one (each app installs and builds alone in CI); keep the two in step.
- **R7** `deploy/backup` `TestScriptsParse` fails on shellcheck warnings in `drill.sh` / `lib-common.sh`; those files are untouched by A10c
  (pre-existing, unrelated).
- Security review of the idempotency wrapper and the three new read shapes is pending (probes below). Accessibility is basics only (as A10b).

## b2c OpenAPI: the 10 mismatches (A10a probe) and their state

A10a recorded 10; three had already been closed on this base (`POST /v1/me/casino/games/{id}/launch` by A8, `GET /v1/me/rg/status` by A6, `POST
/v1/me/deposits` was always documented). The first run of the new b2c contract test found 7, all fixed in the spec (none waived; the b2c allow-list is empty):

1. `GET /v1/me/casino/games` (+ `CasinoGame`) 2. `GET /v1/me/deposits` (+ `DepositIntent`) 3. `GET /v1/me/deposits/{id}` 4. `GET /v1/me/wallets` (+ `WalletSummary`)
5. `GET /v1/me/wallets/{assetCode}` 6. `idempotency_key` (required) on the wager body 7. `idempotency_key` (required) on the win body.

## Changed existing assertions (complete list; none weakened)

Go:
- `internal/httpserver/staff_route_static_test.go`: pin 218 -> 222 routes (mutating stays 135); `staff_route_golden_test.go`: +4 entries (all `none`).
- `internal/httpserver/openapi_adr0114_read_routes_contract_test.go`: the exact mounted/documented set gained the 3 new R1/R2 routes, 2 DTO-vs-schema
  pairs and 2 types in the forbidden-field scan (additions; every old row stays).
- `internal/httpserver/payment_attempts_gate_static_test.go`: R1 registrations 2 -> 3 (the events route, same gate asserted for all three);
  `staffStreamRequest` call sites 2 -> 3 plus a new assertion that the two single-item handlers go through `singleStaffReconciliationID`.
- `internal/tenant/status_readers_static_test.go` + `docs/security/launch-status-readers.md`: the reviewed reader set gained
  `withdrawal_hold_candidates_routes.go` (class A + D, row added).
- `internal/apierror/apierror_test.go`: addition only.

Backoffice (each replaced an assertion about a limitation that no longer exists; the replacement asserts the new behaviour and the old
behaviour is kept where it still applies):
- `ParkedPaymentsPage.test.tsx`: timeline entries 4 -> 6 (two provider events added to the fixture); "no event history" text -> the new
  explanatory text.
- `ReconciliationPage.test.tsx`: "marks which loaded findings belong to the selected run (the API cannot filter by run)" -> replaced for staff
  streams by the run_id filter test; the SAME marking assertions now run for the casino stream (their route has no run filter).
- `HoldResolutionsPage.test.tsx`: "does not show candidate withdrawals" -> the empty-candidates text.
- `api/*.test.ts`: request counts grew with the new calls (additions).
- `idempotency.ts` header comment no longer says the server ignores the key.

b2c: `src/api/rg.contract.test.ts` was renamed to `openapiContract.test.ts` and generalised; it still pins the same 8 rg calls (the `{param}`
placeholder is now the checker's `{}`) and now also checks bodies and query keys for every module.

## Commands and results

Environment: private scratch DB `a10c_t1` (built by `priv_db.sh` from this worktree, migrations through 0164), all dropped at the end.
`gofmt -l cmd internal` clean; `go vet ./...` and `go vet -tags integration ./internal/...` clean;
`golangci-lint run ./...` 0 issues (integration-tagged lint of the new files: 0 issues; the repo's older integration tests carry 639 pre-existing errcheck findings, untouched).

| Command (from the worktree) | Result |
|---|---|
| `go test -count=1 ./...` (unit, no DB) | 66 packages ok, 1 FAIL: `deploy/backup` `TestScriptsParse` (shellcheck on `drill.sh` / `lib-common.sh`; `deploy/` is untouched vs the base `6bf9642`, pre-existing) |
| `PRIV_DB=a10c_t1 PRIV_SRC=<worktree> bash priv_db.sh` then `bash priv_test.sh -tags integration -count=1 -p 1 -timeout 120m ./internal/httpserver ./internal/reconciliation ./internal/capability ./internal/apierror ./internal/tenant ./cmd/...` | all packages ok except ONE httpserver failure, `TestStepUp_GoldenTable_EveryClassifiedRoute` (a coverage pin: reads 83 -> 87); the pin was updated and the test re-run green (next row) |
| same harness, `-v -run 'TestStepUp_GoldenTable\|TestA10c\|TestStaffLiveness\|TestStaffRead\|TestHSEC\|TestCapabilityAPI\|TestPaymentForce\|TestStaffRoutes\|TestOpenAPI\|TestADR0114\|TestPaymentAttemptRoutes\|TestStaffIdempotent\|TestValidIdempotencyKey' ./internal/httpserver` | 116 top-level tests PASS (+38 subtests), 0 FAIL, 0 SKIP (route freeze, golden table, OpenAPI pins, step-up golden, liveness of every golden route, R1/R2 reads, HSEC, K1, K3, the new A10c tests) |
| the 8 A10c integration tests alone (`-run TestA10c`) | 8 PASS, 0 FAIL, 0 SKIP: K1 paging + idempotency, in-progress reservation, K3 idempotency, R1 provider/age filters, age-as-time-predicate, event history, R2 single reads + run_id, HSEC candidates + idempotency (+ 2 DB-free unit tests: key validation, the wrap-exactly-13-routes freeze) |
| migration 0164 down/up round trip on scratch DB `a10c_t2` (`APP_ENV=development migrate down`, then `up`) | rolled back [164], re-applied [164], table present, RLS enabled and forced (`t|t`) |
| `backoffice`: `npm ci`, `npm run build` (= `tsc -b && vite build`), `npm test` | build ok; 56 files, 641 tests PASS, 0 FAIL, 0 SKIP (the shared `api/openapiContract.test.ts` UI-to-OpenAPI test included, not weakened) |
| `b2c`: `npm ci`, `npm run build`, `npm test` | build ok; 24 files, 140 tests PASS, 0 FAIL, 0 SKIP (`openapiContract.test.ts`: 21 tests) |

The b2c contract test was probed with the checker's own negative fixtures (wrong path, wrong method, undocumented body field, unresolved path):
each is detected, so a green run is not vacuous. Scratch DBs `a10c_t1`, `a10c_t2` are dropped at the end of the session.

## Security probe list (for the security review; auth, sessions, admin permissions)

1. Idempotency wrapper: a caller without the route permission, a tenant caller naming another tenant, and a ghost tenant never reserve a row (tested
   for support, a foreign tenant admin and a ghost tenant named by a platform caller).
2. Replay is bound to (tenant, actor, key): another actor with the same key string is not served the stored response (tested); the other tenant's
   RLS scope reads zero rows (tested). Probe: a stolen/replayed access token after session revocation must be refused by `requireLiveStaffSession`
   BEFORE a replay (the wrapper is inside the chain, step-up included).
3. The stored response bodies contain payload hashes and ids the actor already received; check retention/PII expectations for the table (R3).
4. Request pinning: method, path, raw query, raw body. Probe a body that differs only in whitespace or key order (treated as different = 409).
5. Reservation race: two simultaneous identical requests: exactly one acts, the other gets 409 IN_PROGRESS (tested via a seeded in-progress row;
   a true concurrent test is not written).
6. A 5xx or panic path frees the key; a crash after the handler's commit leaves it in_progress (R3). Probe the K3/HSEC decision replay: an approve
   whose response was lost must replay, not act twice (state guards still refuse a second act under a new key).
7. Candidates: tenant_admin/finance/compliance/support all 403, tenant-scoped token with the permission 403, no token 401, ghost tenant 404 with no
   audit row, `tenant_id` query parameter 400; confirm no player/wallet id in the body (tested) and that existing audit metadata carries closed codes only.
8. Events: only closed vocabularies; confirm `decline_stage` / `resolution` / `disposition_at_receipt` never carry provider text (CHECK constraints) and
   that `amount_minor_units` for a tenant viewer is acceptable (they already see the attempt amount).
9. Reconciliation single reads: stream permission before any query (tested 403 for tenant_admin on payment, support everywhere); a mismatch id of
   another stream or tenant is 404; confirm no `reconciliation_key` / expected / actual text (tested).
10. `run_id` filter: canonical lower-case UUID only (400 otherwise); a run of another tenant simply matches nothing under RLS.
11. UI: provider id is validated against the closed charset before any request; the candidate "Request release" button pre-fills only the id (the
    last-6 confirmation is never pre-filled); hostile strings from the new routes render as text through `safeCode`.

## Ledger-finance probe list

1. No ledger, balance, wallet or posting code was touched; no account-model change; no balance read on a bet/settlement path. The new routes read
   `payment_attempts`, `payment_provider_events`, `reconciliation_*`, `withdrawal_requests`, `tenants`, `brands` read-only in READ ONLY transactions.
2. Candidate amounts are `numeric::text` minor units with the registry exponent (`assetregistry.GetAsset`; a missing asset fails the read, 500, never a guess).
3. A candidate listing is not a release: the release still needs the HSEC request, the policy, distinct Persons, the beneficiary exclusion, the
   recount and the signed proof (migration 0124) - untouched. Probe that a withdrawal whose tenant and brand are both active is never listed.
4. Idempotent replay of an HSEC/K3/K1 request or decision cannot double-act: the replay never reaches the handler; under a new key the state
   guards (one pending per attempt, already-decided, counted approvals) refuse. Probe: approve replayed after the resolution executed returns the
   stored 200 (same body) rather than a second posting; confirm that is acceptable for an `executed:true` response.
5. The events route exposes receipt amounts of `succeeded` events only through `amount_minor_units`/`asset_code` (no reference text); confirm this is
   not a second source of truth for reconciliation (it is a view of `payment_provider_events`, append-only).
6. `evidence_ref_hash` semantics are unchanged (still an operator-supplied digest); see the options above.

## Security review (independent, SR10C)

Reviewer: `security` specialist. Scope: the diff `6bf9642..33163c7` (staff idempotency wrapper and migration 0164, the four new
staff read routes, K1 paging, the R1 attempt filters, backoffice wiring, b2c OpenAPI and contract test). Method: code and
migration reading, the A10c / HSEC / K1 / K3 / R1 / R2 / step-up / route-freeze integration suites re-run on a private scratch DB
(`sr10c_sec`, all PASS, dropped afterwards), plus a temporary probe test (not committed) whose results are quoted below. Out of
scope: penetration testing, the unchanged governed flows themselves, the evidence_ref_hash decision (deliverable 8), the ADR 0119
S8 UI. Passing this review does not make the subsystem "secure"; it states what was and was not found.

**Verdict: NOT APPROVED as-is. One Medium finding (SR10C-1) blocks marking deliverable 7 (Idempotency-Key) complete. Deliverables
1-6 and 9-11 are approved on their own.** No launch-blocking finding beyond SR10C-1 (which must be fixed before any launch).

### Verified (no finding)

- `staffIdempotent` wraps exactly the 13 POST routes of `capability_routes.go` (5), `payment_force_resolution_routes.go` (4),
  `withdrawal_hold_resolution_routes.go` (4), always as `staffRoute(..., RequirePermission(...)(staffIdempotent(...)))`; no GET is
  wrapped (static freeze test). `staffRoute` = `auth.Middleware -> RequireStaffPrincipal -> requireLiveStaffSession -> RequireAAL2 ->
  requireStepUp`, so authentication, liveness (revoked session / deactivated staff), aal2, step-up freshness and the static
  permission all run before a key is looked at. Probe: same subject re-presenting the key with a token lacking the permission is
  refused (401/403) with no `Idempotent-Replayed`.
- Scope: unique `(tenant_id, actor_id, idempotency_key)`, tenant = path tenant after `canActOnTenant`, actor = authenticated
  subject, RLS GUC = path tenant. Probes: another platform admin with the same key and body is NOT served the stored body; the same
  actor with the same key on another tenant's path is NOT a replay; a body differing only by one leading space is 409
  `IDEMPOTENCY_MISMATCH`. A refused caller (permission, foreign tenant, ghost tenant via FK 23503) reserves nothing.
- 409 bodies are fixed sentences with no stored data; MISMATCH / IN_PROGRESS are only ever observable by the same actor.
  Concurrency: `INSERT ... ON CONFLICT DO NOTHING` under the unique index serialises two identical requests (one acts, one 409).
- Only 2xx is stored (CHECK 200-299, body <= 256 KiB, both DB-enforced); non-2xx and panic release the reservation. The stored
  bodies are the existing DTOs: ids, closed codes, amounts, payload/evidence hashes, staff ids; no reference text R, no note, no
  player/wallet/destination id, no token or secret. The request body is stored only as a SHA-256 (the K3/HSEC request bodies do
  not carry R; the free-text `note` is only hashed).
- Migration 0164: ENABLE + FORCE RLS, `tenant_id` = `app.tenant_id` and no player GUC, REVOKE ALL FROM PUBLIC, runtime gets
  S/I/U/D only. Down migration is a plain drop (holds no authority or audit evidence).
- New reads: permission gate at registration (events: `payment_force_resolve:read`; run/mismatch single reads: per-stream permission
  in `singleStaffReconciliationID` before any query; candidates: `withdrawal_hold_resolution:read`, platform_admin only, plus a
  `c.platform()` check). Tenant scope by `beginStaffRead`/`canActOnTenant` (foreign tenant 404), platform reads audited first and
  fail closed, ghost tenant 404 without an audit row. RLS on `payment_provider_events`, `reconciliation_runs/_mismatches`,
  `withdrawal_requests` is the strict tenant policy (FORCE). Event DTO fields are DB CHECK-closed vocabularies (`event_type`,
  `outcome`, `disposition_at_receipt`, `resolution`, `decline_stage`); `provider_reference`, `original_provider_reference`,
  `merchant_reference`, `settlement_reference`, `decline_reason` and `event_fingerprint` are not selected. Candidates carry no
  player, wallet or destination id. Unknown query parameters are 400 (`tenant_id` included).
- Pagination: `strictPage` limit 1..200, offset 0..1,000,000; the attempt list stays keyset; `min_age_seconds` 0..31,536,000 with an
  exact-decimal round-trip; `provider_id` is the closed webhook charset; `run_id` canonical lower-case UUID only.
- Backoffice: "Request release" pre-fills `withdrawalId` only; the typed last-6 confirmation (`typed`) starts empty; new strings are
  rendered through `safeCode` / text, no `dangerouslySetInnerHTML`; one Idempotency-Key per dialog (fresh key on reopen).
- b2c contract test: the copied checker differs from the backoffice one only in the directory constant and a comment; the allow-list
  is empty with a stale-entry check; the A6 rg pins are kept; negative fixtures prove path/method/body drift is detected; a scan
  proves no b2c source outside `src/api` calls `fetch`/`apiFetch`. Not weakened.

### Findings

**SR10C-1 (Medium, BLOCKS deliverable 7): stored responses copy acting-session-only rows into a table any tenant-scoped session can
read.** `withdrawal_hold_resolutions` (0124) and `payment_manual_resolutions` (0115), and `staff_capability_grant_requests` in the
acting path (0113), are SELECT-able only with `app.acting_tenant_id` AND `financial_acting_session_valid()`; ADR 0114 12.4 relies on
it (the record itself withholds `open_resolution_id` from candidates for this reason). `staff_idempotency_keys` stores the full
response DTO under a policy that needs only `app.tenant_id`. Probe result: in a plain `WithTenant(tenant)` transaction (no acting
session) `withdrawal_hold_resolutions` returned 0 rows while `staff_idempotency_keys` returned the stored 201 body containing the
resolution id, `payload_hash`, amount, withdrawal id and the requesting PLATFORM staff id (which ADR 0114 H1 redacts from tenant
viewers). Failure scenario: any tenant-scoped code path with a SQL-injection or a mis-scoped query (or a future reader of this table)
reads governed four-eyes state that the database was designed to hide outside an acting session; the DB-level control is silently
bypassed even though no API route exposes it today. Fix (either): (a) bind the policy to the actor through the existing narrow GUC,
`actor_id = NULLIF(current_setting('app.actor_staff_id', true), '')::uuid`, set by the wrapper through
`db.SetActorStaffIDForCurrentTx` (the only setter, static-test pinned, ADR 0119 C2 pattern), and add a probe test that a
tenant-scoped session without it reads zero rows; or (b) store no response body for K3/HSEC/K1 (store status + resource id and answer
a replay with a minimal body / let the client re-read through the governed read routes). (a) is the smaller change. Needs the
architect's sign-off because it reuses a GUC owned by ADR 0119.

**SR10C-2 (Low): stored responses are mutable by the runtime role.** Nothing prevents an UPDATE of a `completed` row's
`response_status`/`response_body`, or a DELETE of a completed row. A buggy or compromised code path could rewrite what is replayed to
an approver (for example a decision response showing `executed:true` / `refused:false`), i.e. DB-level cache poisoning of an
operator-facing result. Fix: a BEFORE UPDATE/DELETE trigger allowing only `in_progress -> completed` (one shot, every other column
frozen) and DELETE only of `in_progress` rows (retention deletes through a separate owner/job path).

**SR10C-3 (Low, accept with documentation): a replay skips the handler's in-handler authority.** The route-level chain re-runs, but
the K3/K1 G-P2 capability-grant check, the acting session and the Person checks live in the handler, which a replay does not call.
A staff member whose capability grant was revoked after the original request can still receive the original 2xx body by replaying
the key. Nothing is acted on and the body is one this actor was already entitled to receive, so the impact is information only; a
replay also writes no audit row (it mutates nothing). Record this in the ADR amendment (SR10C-5) and consider a short retention
(SR10C-4) to bound the window.

**SR10C-4 (Low, availability): no retention and no recovery for an orphaned `in_progress` key** (already residual R3). A crash
between the handler's commit and the completion write leaves the key `in_progress` forever (409 to that dialog). Safe (the console
uses a fresh key on reopen and the flows' state guards refuse a double act), but add a retention job (for example completed rows
older than 24 h, `in_progress` older than 1 h deleted only after confirming no handler is running) before launch.

**SR10C-5 (Low, governance): ADR 0119 6.6 / C8 now contradict the code.** They state the admin four-eyes routes carry no
`Idempotency-Key` and that the client re-sends after step-up without a server claim. A10c introduces a new cross-cutting mechanism on
those routes; the architect must record the amendment (scope: 13 routes, optional header, replay semantics, SR10C-1/3) in
`docs/decisions/`. The record's "Design decisions taken inside the approved architecture" heading understates this.

**SR10C-6 (Info): the R2 single reads have no explicit `tenant_id` predicate** (`GetRunForStream`, `GetMismatchForStream`), contrary
to the record's "plus an explicit `tenant_id` predicate". Isolation holds through the strict FORCE RLS policy (0028), as for the
existing list. Recommended defence in depth: add `AND tenant_id = $n` (as the events and candidate routes do), or correct the record.

**SR10C-7 (Info):** K1 lists with `?offset=` and no `?limit=` return every remaining row (the legacy unbounded behaviour, now
reachable with an offset); tables are per tenant and small, not a DoS on their own. A stored 2xx body over 256 KiB releases the key
after the act (a retry with the same key then hits the state guards, 409); the DTOs are far below that.

### Tests the qa coverage must include (from this review)

1. SR10C-1 regression: after a stored K3 and HSEC response, a `WithTenant(tenant)` session without the actor binding / acting
   session reads 0 rows of `staff_idempotency_keys` (today it reads 1).
2. SR10C-2: UPDATE of a completed row and DELETE of a completed row by the runtime role are refused.
3. Replay after session revocation (`sessions.revoked_at`) is 401 `session_revoked` with no `Idempotent-Replayed` header (the chain
   ordering already guarantees it; pin it with one integration test).
4. Replay after a staff member's G-P2 grant revocation: the documented behaviour (SR10C-3) is pinned, whichever is chosen.
5. A true concurrent pair (two goroutines, same key, same body): exactly one handler run, the other 409 IN_PROGRESS or a replay.
