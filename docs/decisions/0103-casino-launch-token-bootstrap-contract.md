# ADR 0103 — Casino Vendor Launch-Token Bootstrap Contract (CAS-PLAY-BOOTSTRAP-1)

- **Status:** PROPOSED, 2026-09-28. Drafted by `architect` for PRH-2 W0 (workstream W0-X). Nothing
  here is implemented. The implementation is PRH-2 workstream **B** (W2).
- **Decision type:** cross-domain contract: the `casino` domain plus `httpserver`, `webhookauth` and
  the ADR 0097 admission layer.
- **Owner:** `casino`. **Reviewers:**
  - `security` (**hard gate**, S-5);
  - `architect`, `code-reviewer`, `qa` and `product-owner-proxy`;
  - `ledger-finance` only if the §4 lock-order note changes an ADR 0082 order.
- **Registry:** CAS-PLAY-BOOTSTRAP-1. **It depends on CAS-REVOKE-CONSUMED-1 (workstream A,
  migration 0108) having merged** (plan §2, security gate (i)).
- **Binding inputs:** plan §5-B, §2 W2, §4 (the note under the table), §11; `reviews/security.md`
  S-5 and §3 "B"; `reviews/qa.md` W2 "B"; ADR 0025 §3 (the opaque single-use launch token); ADR 0095
  §15.1 (two-phase launch, never-consumed TTL); ADR 0097 (webhook admission); ADR 0091 (uniform 401).
- **Labels:** every decision here is engineering and reversible. No human decision is needed. The
  items for review are in §11.

---

## 1. Context (verified at `cabca27`)

- `CreateLaunchSession` (`internal/casino/launch.go:222-262`) mints a 256-bit token and stores only
  `token_hash` (SHA-256). `casino_launch_sessions.token_hash` is **globally** `UNIQUE` (migration
  0035:160), with FORCE RLS families `tenant_staff_scope` and `player_self_scope` (0035:182,193).
- `ResolveLaunchToken` (`launch.go:279`) has **no non-test caller** and is unsafe for a vendor
  bootstrap (S-5):
  - it selects by hash alone and checks no provider, mode or asset;
  - it lazily writes `expired` (`:311`);
  - it consumes with `WHERE id=$1 AND status='active'` only.

  Checking the binding after that call either burns the session or commits a consume that should
  have been refused.
- `postBet` accepts a bet on an `active` session inside its TTL and on a `consumed` session
  (`orchestrator.go:1483-1488`).
- The 0042 immutability trigger (`0042…up.sql:39-58`) freezes `consumed`, `expired` and `revoked`
  rows. **Workstream A's migration 0108** adds exactly `consumed → revoked`, and makes
  `RevokeLaunchSession` revoke `active` or `consumed`, returning `prior_status`.
- The vendor-facing webhook route shape is `/v1/webhooks/casino/{tenantSlug}/{providerID}`:
  - `webhookPreamble` derives the tenant from the slug and checks it is active;
  - `VerifyCallback` authenticates with the per-tenant credential in a phase-1 read-only
    transaction;
  - ADR 0097 admission gates both (`casino_handlers.go:385-427`).
- Casino has **no dedicated kill-switch table**. The platform-level brakes are:
  - `casino_games.status` (`types.go:83`: "a PLATFORM-level kill switch");
  - `casino_provider_capabilities.status`, `supports_launch` and `supported_assets`
    (`LoadCapability`, used at `orchestrator.go:460-476`).
- **RG** is `rg.EvaluateEligibility` through `evaluateAndAuditEligibility` (`orchestrator.go:706`).
- **Current leak.** `LaunchRequest` passes the raw platform `PlayerAccountID` to the adapter
  (`types.go:547`). This matters for §3.5.

## 2. Decision summary

- Add a new vendor-facing endpoint, the **launch bootstrap**, and a new function
  `casino.BootstrapLaunch` in `internal/casino/bootstrap.go`. `ResolveLaunchToken` is **not
  reused**, as-is or wrapped. It stays test-only; deleting it is §11 item 5.
- The consume is a **binding-aware CAS**: the provider, tenant, mode, asset and expiry predicates
  are **inside** the `UPDATE … WHERE`, never checked after it.
- The contract, in S-5's order, is in §3.

## 3. Contract

### 3.1 Request (MOCK/vendor → platform)

- **Route:** `POST /v1/webhooks/casino/{tenantSlug}/{providerID}/launch-bootstrap`. The final path
  string is the casino implementer's choice, but it must share the webhook preamble.
- The tenant and provider come **from the route**, validated by the preamble and verified by
  `webhookauth`. **The body never carries a tenant.** Unknown fields are refused.
- **Body** (signed exactly like a callback):
  - `launch_token`: the raw token;
  - `request_id`: `^[A-Za-z0-9_.:-]{1,128}$`;
  - `provider_game_id`, `asset_code`, `mode`: the vendor's view of the launch, each compared to the
    session.

### 3.2 The single-transaction order (S-5 steps 1–7)

**Step 1: authenticate, outside any transaction.**
- Run the preamble (charset, bounded body, tenant by slug, tenant active), ADR 0097 admission,
  then `VerifyCallback`-equivalent `webhookauth` verification (ADR 0094 §4.1 phase 1, short
  read-only), then `admitVerified`.
- Any failure → the **existing uniform 401** "callback rejected".
- `tenantID` and `providerID` are now server-derived.

Then `deps.DB.WithTenant(tenantID, …)` opens **one** transaction:

**Step 2: look up the session.**
- `SELECT … FROM casino_launch_sessions WHERE token_hash = $h AND tenant_id = $t FOR UPDATE`,
  where `$h = hashLaunchToken(raw)`.
- RLS already scopes this to the tenant; the explicit `tenant_id` predicate is defence in depth.
- Before the binding checks, look up the idempotency row by `(tenant_id, provider_id,
  request_id)`. If it exists, take the §3.4 replay branch.

**Step 3: check the binding.** Refuse **uniformly** (§3.6) unless the row exists and **all** of the
following hold:
- `provider_id = $providerID`;
- `tenant_id = $t`;
- `mode = body.mode`;
- `asset_code = body.asset_code`;
- `provider_game_id = body.provider_game_id`;
- `status = 'active'`;
- `expires_at > now()`.

On refusal, **nothing is written**: no status change, no lazy `expired` write, no idempotency row.
The transaction rolls back.

**Step 4: re-check the gates.** These are read in the same transaction:
- the game is `active`;
- the capability is `active`, with `supports_launch`, `supports_bet` when `mode = real`, and
  `asset_code ∈ supported_assets`;
- RG eligibility, through `evaluateAndAuditEligibility`, which audits a denial itself.

On denial, see §3.3.

**Step 5: CAS.**
```sql
UPDATE casino_launch_sessions
   SET status = 'consumed', consumed_at = now()
 WHERE id = $id AND token_hash = $h AND tenant_id = $t AND provider_id = $p
   AND mode = $m AND asset_code = $a AND status = 'active' AND expires_at > now()
RETURNING id
```
Zero rows → the uniform refusal, and the transaction rolls back.

**Step 6: write the idempotency record.**
- Upsert the provider-scoped player ref (§3.5).
- INSERT the `casino_launch_bootstraps` row (§5), keyed `(tenant_id, provider_id, request_id)`,
  bound to `token_hash` and `launch_session_id`, and storing the response body.
- A unique violation (a concurrent same-`request_id` race) → the transaction rolls back; the handler
  retries **once** and takes the replay branch (§3.4).

**Step 7: audit, then commit.**
- Write `audit.Record` with:
  - `action = "casino.launch_bootstrapped"`, `actor_type = system`;
  - `target_type = casino_launch_session`, `target_id = session id`;
  - metadata `{provider_id, request_id, mode, asset_code}`.
- The metadata contains **no raw token and no token hash** (migration 0014 metadata rule; `audit.go:50-53`).

After the commit, respond `200 {session_id, player_ref, provider_game_id, asset_code, mode}`.

### 3.3 A refusal never consumes; a gate denial **revokes**

| Refusal | Session afterwards | Why |
|---|---|---|
| Steps 1–3 (auth, not found, wrong provider/tenant/mode/asset/game, not active, expired) | **Unchanged** (`active` stays `active`; nothing is written) | The caller has not proven it holds a **bound** token for its own provider. Letting it mutate the row would let one authenticated vendor burn another vendor's sessions (DoS) and would turn writes into an oracle. |
| Step 4 gate denial (game or capability off, RG ineligible) | **`revoked`**, via `RevokeLaunchSession` (post-A: prior status `active`, `prior_status` audited), then commit | The caller **has** proven the binding, so the denial is a considered platform decision about this launch. If the session were left `active`, `postBet` would still accept bets on it for the rest of its TTL (`orchestrator.go:1483-1488`), and the vendor could retry the bootstrap until a transient gate flips. Revoking is fail-closed and costs the player only a relaunch (a new token). It is a revoke, not a consume: no idempotency row and no player ref are written. |

The step 4 denial returns `403 {"error":"launch not permitted"}`. It is distinguishable from the
uniform refusal only by a caller that has already passed step 3, so it is not an existence oracle.
**The RG, game or capability reason is never disclosed to the vendor**: RG status is player-
sensitive. The denial reason goes only into the audit row, as the RG denial audit and the revoke
audit.

### 3.4 Replay (bound to `token_hash` and to `consumed`)

For an existing `casino_launch_bootstraps` row with the same `(tenant_id, provider_id,
request_id)`:

| Condition | Result |
|---|---|
| Stored `token_hash` = presented hash **and** the session is currently `consumed` | Return the **stored** response, 200, byte-identical. No write. Logged at Info `casino_launch_bootstrap_replayed`. |
| Stored `token_hash` ≠ presented hash (a reused request id with a different token) | Uniform refusal. No write. |
| Same hash, but the session is now `revoked` (A's `consumed → revoked`, e.g. a staff safety revoke) | Uniform refusal. **The replay does not resurrect a revoked session.** |

- A **different** `request_id` for an already-consumed token fails at step 3 (`status <> 'active'`)
  and gets the uniform refusal: single use.
- `UNIQUE (launch_session_id)` on the idempotency table means at most one successful bootstrap per
  session exists, even if the CAS predicate regressed (§5).

### 3.5 Player reference: opaque and provider-scoped

- The response carries `player_ref`: a random UUID from `casino_provider_player_refs`, UNIQUE per
  `(tenant_id, provider_id, player_account_id)`. It is created on first bootstrap in step 6 with
  `INSERT … ON CONFLICT DO NOTHING`, then read.
- It is stable per player per provider, and unlinkable across providers and tenants.
- It carries no PII and no platform id. It is not derived from any secret, so it needs no key
  management and survives key rotation.
- **Known gap (not closed by B):** phase B's `LaunchRequest` still hands the adapter the raw
  `PlayerAccountID` (`types.go:547`, a file outside B's Touches). Until that changes, the vendor can
  link `player_ref` to the platform id. See §11 item 1.

### 3.6 Uniform refusal and secrecy

- **One response for every step 1–3 failure and every non-matching replay:** `401 {"error":
  "callback rejected"}`, the same as the existing webhook contract (ADR 0091). Existing, wrong
  provider, wrong tenant, expired, consumed, revoked and unknown are indistinguishable to the
  caller.
- **Logging.** Server-side logs may carry a closed `reason` enum (`not_found`, `binding_mismatch`,
  `not_active`, `expired`, `replay_hash_mismatch`, `replay_revoked`) plus `tenant_id`,
  `provider_id` and `request_id`.
  - **The raw token is never logged, audited, traced or put in an error string.**
  - Its hash is not logged either (`audit.go:50-53`).
  - The request body is never logged.
- **Timing.** No attempt is made at constant-time equivalence across refusal classes; this is a
  residual, §11 item 4. Every step 1–3 refusal performs the same single indexed lookup and writes
  nothing.

## 4. Concurrency and lock order

- `FOR UPDATE` on the session row serializes concurrent bootstraps of the same token.
  - The loser, with a different `request_id`, sees `consumed` → uniform refusal.
  - The loser with the **same** `request_id` hits the idempotency UNIQUE or the replay branch →
    the stored 200.
  - **Exactly one consume.**
- **Lock order.** Step 2 locks the session row **before** RG's advisory locks (step 4). `postBet`
  reads the session **without** `FOR UPDATE`. The casino implementer must add the bootstrap to the
  existing lock-order harness (`casino/lockorder_harness_test.go`) and confirm that no cycle exists
  with `LaunchGame` phase C or `postBet`. If a cycle is found, the fix is to move RG ahead of the
  row lock, and that goes back to architect and security before merge.

## 5. Migration: YES, one is needed (number NOT allocated here; the orchestrator allocates at merge, plan §3 Rule 3)

The migration adds two tables. Both have `FORCE ROW LEVEL SECURITY` and a **tenant family**:
`tenant_id = NULLIF(current_setting('app.tenant_id',true),'')::uuid`, with `app.player_account_id`
**and** `app.platform_admin_principal_id` unset (the 0106 mixed-GUC exclusion). Both are append-only:
UPDATE and DELETE are denied per row, and TRUNCATE per statement (the 0014/0016 pair).

**`casino_launch_bootstraps`:**

| Column | Constraints |
|---|---|
| `id` | |
| `tenant_id` | |
| `provider_id` | |
| `request_id` | CHECK charset/length |
| `token_hash` | |
| `launch_session_id` | Composite FK `(launch_session_id, tenant_id)` to the session; add the supporting UNIQUE if one is absent |
| `player_ref` | |
| `response` | JSONB, ≤ 1 KiB, keys ⊆ the §3.2 response keys |
| `created_at` | |

- `UNIQUE (tenant_id, provider_id, request_id)` and `UNIQUE (launch_session_id)`.
- **BEFORE INSERT trigger** (defence in depth): the referenced session has the same `tenant_id`,
  `provider_id = NEW.provider_id`, `token_hash = NEW.token_hash` and `status = 'consumed'`.

**`casino_provider_player_refs`:**
- Columns: `tenant_id`, `provider_id`, `player_account_id` (FK), `player_ref UUID DEFAULT
  gen_random_uuid()`, `created_at`.
- `UNIQUE (tenant_id, provider_id, player_account_id)` and `UNIQUE (tenant_id, provider_id,
  player_ref)`.

**Other rules:**
- No change to `casino_launch_sessions` or its trigger. A owns that, through 0108.
- Grants: append-only lines in `deploy/init-app-role.sql`.
- **Down:** refuse while any row exists in either table.

## 6. Dependency on workstream A (migration 0108)

B does not start until A has merged (plan §2 and §11). B relies on:
- `RevokeLaunchSession` covering `active` and `consumed` and returning `prior_status` (the §3.3
  gate-denial revoke and its audit);
- `consumed → revoked` existing, so §3.4 "replay after revoke is refused" is testable;
- A's inverted characterization test (`launch_two_phase_integration_test.go:703-795`) staying
  green.

## 7. MOCK over HTTP; no in-process shortcut

- `MockCasinoProvider` (`casino/mock.go`) gains a **bootstrap client**. It builds the signed
  bootstrap request with the same per-tenant derived key as `CallbackPayload`/`SignRawBody`, and
  tests send it **over HTTP** to the real route on an `httptest` server.
- No test and no production code calls `BootstrapLaunch` in-process to stand in for the vendor. The
  only in-process calls are the function's own unit tests, and even those use a real
  `WithTenant` transaction.
- The never-consumed TTL rule and the synthetic-adapter outbound tripwire are unchanged.
- **Label: MOCK.** A real vendor bootstrap is PROVIDER DEPENDENT: its field names and signing
  scheme map onto this contract in the vendor's adapter.

## 8. Invariants (for `qa` and `code-reviewer`)

| ID | Invariant |
|---|---|
| BS-1 | The binding predicates are inside the consuming `UPDATE`. There is no read-then-check-then-consume. |
| BS-2 | A step 1–3 refusal writes nothing. A step 4 denial only revokes (plus audits). Neither creates an idempotency row. |
| BS-3 | At most one consume per session; at most one bootstrap row per session (the DB UNIQUE). |
| BS-4 | A replay returns the stored response only when the hash matches and the session is `consumed`. |
| BS-5 | The tenant and provider come from the route and credential only. The body never names a tenant. |
| BS-6 | The raw token and its hash never appear in logs, audit rows, errors or traces. |
| BS-7 | `player_ref` is opaque, random and provider-scoped. |
| BS-8 | Every step 1–3 refusal and non-matching replay has one byte-identical response. |

## 9. Tests (plan §5-B, QA W2 "B"; T-1/T-2 apply)

- **ADV, one per S-5 step,** each asserting the uniform 401 and **session unchanged, no rows
  written:**
  - an unsigned or wrongly signed request;
  - an unknown tenant slug;
  - a token for tenant B presented on tenant A's route (cross-tenant);
  - a token for provider P1 presented by authenticated provider P2 (cross-provider);
  - a mode, asset or game mismatch;
  - an expired token (the fixture writes `expires_at` in the past; there is no sleep);
  - an already-consumed token with a new `request_id`;
  - a revoked token;
  - **replay after A's `consumed → revoked`** → refused;
  - **a reused `request_id` with a different token hash** → refused;
  - a body carrying a `tenant_id` field → refused as an unknown field.
- **Refusal-does-not-consume:** every step 1–3 case above, followed by the legitimate request →
  success.
- **Gate denial:** the game disabled, the capability disabled, or the player RG-excluded → 403,
  session `revoked`, `prior_status=active` audited, no bootstrap row, and a later `postBet` on that
  session is refused.
- **CON:** two concurrent consumes of the same token with different `request_id`s → **exactly one**
  200 and one uniform refusal; one bootstrap row; the status is `consumed` once. Run under `-race`
  at `-count=50`. The same test with the same `request_id` → two 200s with identical bodies and one
  row.
- **IDM:** replay → a byte-identical stored response and no new audit row.
- **AU:** one `casino.launch_bootstrapped` row per success. A scan for the raw token and the
  `token_hash` across all audit rows and captured logs finds nothing.
- **TI:** tenant A's route never resolves B's token. `player_ref` differs across providers and
  across tenants for the same person.
- **MIG:** the insert trigger refuses a row whose hash or provider does not match its session, and
  one whose session is not `consumed`; UPDATE, DELETE and TRUNCATE are refused; `down` refuses
  while rows exist.
- **Lock order:** the bootstrap is added to the casino lock-order harness.
- **MUT (must be killed):**
  - drop `provider_id` from the CAS predicate;
  - drop `expires_at > now()` from the CAS;
  - drop `status='active'` from the CAS;
  - move the provider check after the CAS;
  - replay without the hash comparison;
  - replay without the `consumed` check;
  - write a lazy `expired` on refusal;
  - drop `UNIQUE (launch_session_id)`.

## 10. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| Reuse `ResolveLaunchToken`, and check the binding afterwards | S-5: this burns sessions or commits a wrong consume. |
| Leave the session `active` on a gate denial | `postBet` would still accept bets on it within its TTL, and the vendor could retry until the gate flips (§3.3). |
| Revoke on a step 3 binding mismatch | A cross-vendor DoS and a write oracle. |
| HMAC-derived `player_ref` | Needs a new secret with rotation that breaks stability. The stored random mapping is simpler. |
| UUIDv5 `player_ref` over `player_account_id` | Reversible by anyone holding the platform id, which the vendor currently does (§3.5). |
| Store the idempotency data on `casino_launch_sessions` | Needs 0042/0108 trigger changes on a table A owns in the same round. A separate append-only table is cleaner and gives `UNIQUE (launch_session_id)` for free. |
| Player-authenticated bootstrap | The bootstrap is vendor→platform by definition (ADR 0025 §3). |

## 11. Open items

1. **`LaunchRequest.PlayerAccountID` (`types.go:547`).** Recommend registering
   **CAS-PLAYER-REF-1**: phase B should pass the provider-scoped `player_ref` instead of the platform
   id. This requires minting the ref at launch rather than at bootstrap. It touches
   `casino/types.go` and `orchestrator.go`, outside B's Touches, so the orchestrator should schedule
   it.
2. **`postBet` still accepts bets on a never-bootstrapped `active` session within its TTL**
   (`orchestrator.go:1483-1488`). Whether a real-money bet should require a `consumed` session is a
   casino and security question; it is recommended for registry review. The CAS-PLAY-BOOTSTRAP-1
   note "do NOT relax the active-session expiry" stands.
3. **Risk and jurisdiction** are not re-evaluated at bootstrap. They are re-evaluated on every bet
   in `postBet`. **Security to confirm** that the S-5 step 4 set (the kill-switch equivalents,
   capability and RG) is sufficient.
4. **Timing side-channel** between refusal classes: disclosed residual; security to rule whether it
   matters behind the authenticated-caller requirement.
5. **Delete `ResolveLaunchToken`** once B lands and its tests migrate. That is casino's call, and it
   is not required by this ADR.

**Handover/DoD:**
- **Artefacts:**
  - this ADR ACCEPTED after security review;
  - A merged first;
  - B merged with the §5 migration (number allocated by the orchestrator), `casino/bootstrap.go`,
    the route, the MOCK bootstrap client and the §9 tests;
  - a `docs/integrations/` contract page (vendor view: request, response and error classes, no
    internals);
  - HANDOVER mock-vs-real row: "casino bootstrap: MOCK over HTTP; real vendor PROVIDER DEPENDENT".
- **Registry (orchestrator):** CAS-PLAY-BOOTSTRAP-1 → IMPLEMENTED (MOCK); register CAS-PLAYER-REF-1.
