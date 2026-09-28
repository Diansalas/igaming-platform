# ADR 0103 — Casino Vendor Launch-Token Bootstrap Contract (CAS-PLAY-BOOTSTRAP-1)

> **Status 2026-09-28: ACCEPTED** (revision 2). Ledger-finance and security accepted it: see `docs/plans/prh2-hardening-round/reviews/` (`adr-0102-0104-security-confirmation.md`, and for 0102 also the confirmation section of `adr-0102-ledger-finance.md`); product-owner-proxy accepted it in `adr-0102-0104-product-owner-proxy.md`. Implementation is NOT IMPLEMENTED until the PRH-2 workstream merges. The earlier PROPOSED status below is historical.

- **Status:** PROPOSED, **revision 2**, 2026-09-28. Drafted by `architect` for PRH-2 W0 (W0-X).
  Nothing here is implemented. The implementation is PRH-2 workstream **B** (W2).
- **Revisions:**

  | Rev | Base | Change |
  |---|---|---|
  | 1 | `f06818b` | Initial draft |
  | 2 | `10e0471` | Applies `reviews/adr-0102-0104-security.md` (ACCEPT WITH CONDITIONS: SB-1..SB-8, Q4–Q6, C-103-1..6) and the orchestrator's resolutions (SB-1, SB-2, SB-3, SB-6, SB-7). Product-owner-proxy: ACCEPT. Mapped in §12 |

- **Decision type:** cross-domain contract: `casino`, `httpserver`, `webhookauth`, and ADR 0097
  admission.
- **Owner:** `casino`. **Reviewers:**
  - `security` (**hard gate**);
  - `architect`, `code-reviewer`, `qa`, `product-owner-proxy`;
  - `ledger-finance`, only if §4 changes an ADR 0082 order.
- **Registry:** CAS-PLAY-BOOTSTRAP-1. It depends on **CAS-REVOKE-CONSUMED-1 (workstream A,
  migration 0108) having merged**. Related rows, already registered:
  - CAS-PLAYER-REF-1: must close **before any real vendor** (SB-8);
  - CAS-BET-REQUIRES-BOOTSTRAP-1 (SB-5);
  - CAS-GAME-KILL-BET-1 (SB-4): blocks real-money casino launch, but not B.
- **Binding inputs:** plan §5-B, §2 W2, §4 note, §11; security S-5 and the revision-2 review; QA
  W2 "B"; ADR 0025 §3; ADR 0094 §4.1/§5 (two-phase verification, Redeem+Recheck); ADR 0095 §15.1;
  ADR 0097; ADR 0091; ADR 0099 §6 (`app.acting_*` GUCs).
- **Labels:** engineering and reversible. No human decision is needed.

---

## 1. Context (verified at `cabca27`)

- `CreateLaunchSession` (`internal/casino/launch.go:222-262`) stores only `token_hash`. That column
  is **globally** UNIQUE (0035:160), and the table has FORCE RLS families `tenant_staff_scope` and
  `player_self_scope` (0035:182,193).
- `ResolveLaunchToken` (`launch.go:279`) has no non-test caller, and it is unsafe for a vendor
  bootstrap (S-5):
  - it looks up by hash alone;
  - it checks no provider, mode or asset;
  - it lazily writes `expired` (**`:314`**);
  - its consume predicate is `id` + `status` only.
- **`postBet`** accepts bets on `active` sessions within their TTL and on `consumed` sessions
  (`orchestrator.go:1483-1488`). On every bet it re-checks:
  - the provider binding;
  - capability;
  - RG;
  - risk, evaluated with the session's frozen `jurisdiction_code`.

  It does **not** re-run jurisdiction resolution, the game blocklist or `casino_games.status`
  (SB-3). The last gap is registered as CAS-GAME-KILL-BET-1 (SB-4).
- The 0042 trigger freezes terminal rows. **Migration 0108 (A)** adds exactly `consumed → revoked`,
  and `RevokeLaunchSession` then handles `active` or `consumed` and returns `prior_status`.
- **Webhook route shape:** `/v1/webhooks/casino/{tenantSlug}/{providerID}`.
  - `webhookPreamble` resolves the tenant.
  - `VerifyCallback` is phase 1: no transaction, then a short read-only credential read.
  - The domain transaction begins with **Redeem+Recheck** of the verified handle as its first
    statement (`orchestrator.go:1020-1027`, via `webhook_verify.go:124`; ADR 0094 §5). The adapter
    parses only the verified bytes.
  - ADR 0097 admission gates both phases (`casino_handlers.go:385-427`).
- **Casino has no kill-switch table.** The platform brakes are:
  - `casino_games.status` (`types.go:83`);
  - `casino_provider_capabilities`: `status`, `supports_launch`, `supports_bet`,
    `supported_assets` (`orchestrator.go:460-476`).
- RG runs through `evaluateAndAuditEligibility` (`orchestrator.go:706`).
- `LaunchRequest` passes the raw `PlayerAccountID` to the adapter (`types.go:547`), which is
  CAS-PLAYER-REF-1.

## 2. Decision summary

- Add a new vendor-facing endpoint backed by `casino.BootstrapLaunch` (`internal/casino/bootstrap.go`).
- **`ResolveLaunchToken` is not reused**, either as-is or wrapped.
- The consume is a **binding-aware CAS**. The provider, tenant, mode, asset, **provider game** and
  expiry predicates all sit inside the `UPDATE … WHERE` (SB-2).
- The transaction's **first statement is Redeem+Recheck** of the verified credential handle (SB-1).

## 3. Contract

### 3.1 Request

- **Route:** `POST /v1/webhooks/casino/{tenantSlug}/{providerID}/launch-bootstrap`. The final path
  is the implementer's choice, but it must share the preamble.
- The tenant and provider come from the route and are verified by `webhookauth`. **The body never
  carries a tenant**, and unknown fields are refused.
- **Signed body:**
  - `launch_token`;
  - `request_id` (`^[A-Za-z0-9_.:-]{1,128}$`);
  - `provider_game_id`, `asset_code`, `mode`.
- The body is parsed **only from the verified bytes** that Redeem returns (SB-1).

### 3.2 The single-transaction order (S-5 steps 1–7, with SB-1)

**Step 1: authenticate, outside any transaction.**
- Run the preamble, ADR 0097 pre-auth admission, and phase-1 `webhookauth` verification (ADR 0094
  §4.1: a short read-only credential read that commits before any secret-store fetch). Then run
  `admitVerified`.
- Any failure → the existing **uniform 401** "callback rejected".

Then `deps.DB.WithTenant(tenantID, …)` opens **one** transaction:

**Step 2a: Redeem+Recheck first (SB-1, C-103-1).**
- The first statement is Redeem+Recheck of the verified handle (the `redeemVerified` pattern,
  `orchestrator.go:1027`).
- A handle that was revoked, expired or rotated away between phase 1 and this transaction, or a
  tenant/provider/domain mismatch, → the uniform 401. **Nothing is read or written.**
- The body is then parsed **only from the verified bytes** that Redeem returns.

**Step 2b: look up the session and the idempotency row.**
- `SELECT … FROM casino_launch_sessions WHERE token_hash = $h AND tenant_id = $t FOR UPDATE`.
- Look up the idempotency row by `(tenant_id, provider_id, request_id)`. If it exists, take the
  §3.4 replay branch.

**Step 3: check the binding.** Refuse **uniformly** unless the row exists and **all** of these hold:
- `provider_id = $p`;
- `tenant_id = $t`;
- `mode`, `asset_code` and `provider_game_id` equal the body's values;
- `status = 'active'`;
- `expires_at > now()`.

On refusal, **nothing is written** (no lazy `expired`) and the transaction rolls back.

**Step 4: re-check the gates**, in the same transaction:
- the game is `active`;
- the capability is `active`, with `supports_launch`, `supports_bet` for `real`, and the asset in
  `supported_assets`;
- RG eligibility (`evaluateAndAuditEligibility`).

On denial, see §3.3.

**Step 5: CAS (SB-2).**
```sql
UPDATE casino_launch_sessions
   SET status = 'consumed', consumed_at = now()
 WHERE id = $id AND token_hash = $h AND tenant_id = $t AND provider_id = $p
   AND mode = $m AND asset_code = $a AND provider_game_id = $g
   AND status = 'active' AND expires_at > now()
RETURNING id
```
Zero rows → the uniform refusal, and the transaction rolls back.

**Step 6: write the idempotency record.**
- Upsert the player ref (§3.5).
- INSERT `casino_launch_bootstraps` (§5) with:
  - `(tenant_id, provider_id, request_id)`;
  - `token_hash`;
  - **`request_digest`** (SB-6, §3.4);
  - `launch_session_id`;
  - the response body.
- A unique violation → roll back, retry **once**, and the retry takes the replay branch.

**Step 7: audit, then commit.**
- `casino.launch_bootstrapped`, `actor_type=system`, target = the session, metadata
  `{provider_id, request_id, mode, asset_code}`.
- **No raw token and no token hash** in the audit row (`audit.go:50-53`).
- Respond `200 {session_id, player_ref, provider_game_id, asset_code, mode}`.

### 3.3 A refusal never consumes; a gate denial **revokes** (Q6 ACCEPTED with conditions, C-103-3)

| Refusal | Session afterwards | Why |
|---|---|---|
| Steps 1–3, including Redeem+Recheck (auth, not found, binding mismatch, not active, expired) | **Unchanged.** Nothing is written | The caller has not yet proven that it holds a bound token for its own provider. Letting it write would allow a cross-vendor DoS and give it an oracle |
| Step 4 **definitive** gate denial (game or capability off, RG ineligible) | **`revoked`** via `RevokeLaunchSession`, then commit | The binding has been proven, so the denial is a considered decision. If the session stayed `active`, `postBet` would still accept bets within the TTL, and the vendor could retry until a transient gate flipped |

Conditions from Q6 / C-103-3:
- **Only a definitive denial revokes.** An **evaluation error** (a DB error, or an RG/capability
  lookup failure) rolls back and returns **5xx**, and the session is untouched.
- The revoke must return `true`. A `false` return under the row lock means an invariant was broken:
  roll back and return 5xx.
- **Audit in the same transaction:** `casino.launch_bootstrap_denied`, `actor_type=system`, with
  `prior_status` and a closed **reason enum** (`game_inactive`, `capability_denied`, `rg_ineligible`).
  RG's own denial audit is also written by `evaluateAndAuditEligibility`.
- **The 403 body is constant:** `{"error":"launch not permitted"}`. The reason is never sent to the
  vendor, because RG status is player-sensitive.
- The revoke writes no idempotency row and no player ref.

### 3.4 Replay (bound to `token_hash`, `request_digest` and `consumed`)

- **Request digest (SB-6, C-103-4).**
  `request_digest = SHA-256(canonical(provider_id, request_id, provider_game_id, asset_code, mode))`,
  computed over the verified, parsed fields. The token is bound separately by `token_hash`, so the
  digest carries no token material.
- For an existing `(tenant_id, provider_id, request_id)` row:

  | Condition | Result |
  |---|---|
  | `token_hash` matches **and** `request_digest` matches **and** the session is currently `consumed` | Return the stored response: 200, byte-identical. No write. Log `casino_launch_bootstrap_replayed` at Info |
  | The hash or the digest differs (a reused request id with a different token or different fields) | Uniform refusal. No write |
  | Same hash and digest, but the session is now `revoked` (A's `consumed → revoked`) | Uniform refusal. **The replay does not resurrect a revoked session** |

- A different `request_id` for an already-consumed token fails at step 3 (single use).
- `UNIQUE (launch_session_id)` means at most one bootstrap row per session, even if the CAS
  regressed.

### 3.5 Player reference: opaque and provider-scoped

- `player_ref` is a random UUID from `casino_provider_player_refs`, UNIQUE per
  `(tenant_id, provider_id, player_account_id)`. It is created on first bootstrap.
- It is stable per player and provider, unlinkable across providers and tenants, and derived from
  no secret.
- **Known gap:** phase-B `LaunchRequest` still sends the raw `PlayerAccountID` (`types.go:547`).
  **CAS-PLAYER-REF-1 must close before any real vendor** (SB-8). Until then the opacity holds only
  against a vendor that does not also receive the Launch call.

### 3.6 Uniform refusal and secrecy (Q5 ACCEPTED)

- **One response** for every step 1–3 failure (including Redeem+Recheck) and every non-matching
  replay: `401 {"error":"callback rejected"}` (ADR 0091).
- **Server-side logs** carry a closed `reason` enum (`not_found`, `binding_mismatch`, `not_active`,
  `expired`, `replay_mismatch`, `replay_revoked`, `credential_unavailable`), plus `tenant_id`,
  `provider_id` and `request_id`.
  - **The raw token is never logged, audited, traced or put in an error string** (C-103-5 scans the
    new error paths, §9).
  - The token hash and the request body are never logged either.
- **Timing (Q5).** Every step 1–3 refusal performs the same lookups, writes nothing and uses no
  class-specific sleep. The residual timing difference is accepted. **Security re-rules if** the
  endpoint ever becomes reachable without authentication, or if token entropy drops below 256 bits.

## 4. Concurrency and lock order

- `FOR NO KEY UPDATE` on the session row serializes concurrent bootstraps of one token:
  - a different `request_id` → the uniform refusal;
  - the same `request_id` → the replay 200;
  - **exactly one consume** either way.
- **Lock order.** Redeem+Recheck (credential tables, read), then the session row lock, then RG's
  advisory locks.
- **Amendment (2026-09-28, architect review F-1, `docs/plans/prh2-hardening-round/reviews/
  b-architect-qa-pop.md`):** the original design and its first implementation (`e720001`) locked
  the session row `FOR UPDATE`. That was refuted: `postBet`'s first bet of a round takes RG's
  person-scoped advisory lock (`internal/rg` `lockPerson`) BEFORE it calls `BindProviderRound`,
  whose `INSERT` into `casino_provider_rounds` carries a foreign key to `casino_launch_sessions`
  (migration 0080) - Postgres takes an *implicit* `FOR KEY SHARE` lock on the referenced session
  row to enforce that FK, with no explicit SQL naming it anywhere in `postBet`. Bootstrap's
  session-row-then-RG order and `postBet`'s RG-then-session-FK order are the same two resources in
  opposite sequence: a genuine ABBA deadlock (`40P01`) between a bootstrap and the first bet of a
  round on the same `active` session. It is a liveness defect only - the losing side's whole
  transaction rolls back, so there is no ledger effect, and Postgres's own deadlock detector
  always resolves it (one side aborts, the other completes) rather than hanging forever.
  - **Fix:** `getLaunchSessionForBootstrap`'s session lookup uses `FOR NO KEY UPDATE`, not
    `FOR UPDATE`. Per Postgres's row-lock compatibility table, `FOR KEY SHARE` (what the FK takes)
    does **not** conflict with `FOR NO KEY UPDATE` - only with `FOR UPDATE` and with another
    `FOR NO KEY UPDATE` holder. The fix therefore breaks the cycle without weakening any of the
    serialization this section originally required: two concurrent bootstraps still conflict
    (`FOR NO KEY UPDATE` vs. `FOR NO KEY UPDATE`), and a bootstrap still conflicts with A's own
    `RevokeLaunchSession` (`internal/casino/launch.go`, a plain `UPDATE`, which takes an implicit
    `FOR UPDATE`-strength lock and so still conflicts with `FOR NO KEY UPDATE`).
  - **Test:** `TestLockOrder_BootstrapAndFirstBetOfRound_NoDeadlock`
    (`internal/casino/lockorder_integration_test.go`) drives both sides through the real
    production primitives (this function's own lock statement, parameterised by lock clause, and
    the real `rg.EvaluateEligibility`/`BindProviderRound` calls) rather than stand-ins. Its
    "`FOR UPDATE` (mutant, pre-fix)" sub-test reproduces the `40P01` this amendment describes;
    its "`FOR NO KEY UPDATE` (fix)" sub-test - the code's actual, current lock clause - completes
    both racers with no deadlock. Security acknowledged this fix on the same review round.
- `postBet` reads the session without any lock of its own; the FK-driven `FOR KEY SHARE` above is
  the only lock it takes on that row, and it is implicit.
- The bootstrap is added to `casino/lockorder_harness_test.go` (the shared interleaving primitives)
  and exercised by `casino/lockorder_integration_test.go` (the actual `Test...` function, this
  codebase's established split - see that file's own package comment). If a cycle reappears with
  `LaunchGame` phase C or `postBet`, the fix is to move RG ahead of the row lock, and that goes back
  to architect and security.

## 5. Migration: YES (number NOT allocated here; the orchestrator allocates at merge, Rule 3)

**Common properties of both tables:**
- `FORCE ROW LEVEL SECURITY`, **tenant family**:
  `tenant_id = NULLIF(current_setting('app.tenant_id',true),'')::uuid`, **with all of these unset**
  (the 0106 pattern extended; SB-7, C-103-5):
  - `app.player_account_id`;
  - `app.platform_admin_principal_id`;
  - `app.platform_service_id`;
  - `app.acting_tenant_id`;
  - `app.acting_platform_principal_id`.
- Append-only: UPDATE and DELETE denied per row, TRUNCATE per statement.

**`casino_launch_bootstraps`:**
- Columns: `id`, `tenant_id`, `provider_id`, `request_id` (charset CHECK), `token_hash`,
  **`request_digest`** (a 64-hex CHECK), `launch_session_id` (composite FK with `tenant_id`; add
  the supporting UNIQUE on the sessions table if it is absent), `player_ref`, `response` (JSONB,
  ≤ 1 KiB, keys ⊆ the §3.2 response keys), `created_at`.
- Constraints: `UNIQUE (tenant_id, provider_id, request_id)` and `UNIQUE (launch_session_id)`.
- **BEFORE INSERT trigger:** the session has the same tenant, the same provider, the same
  `token_hash`, and `status = 'consumed'`.

**`casino_provider_player_refs`:**
- Columns: `tenant_id`, `provider_id`, `player_account_id` (FK),
  `player_ref UUID DEFAULT gen_random_uuid()`, `created_at`.
- UNIQUE on `(tenant, provider, player_account_id)` and on `(tenant, provider, player_ref)`.

**Other rules:**
- No change to `casino_launch_sessions` or its trigger; A owns them.
- Grants: append-only lines in `deploy/init-app-role.sql`.
- **Down:** refuse while rows exist.

## 6. Dependency on workstream A (migration 0108)

B starts only after A merges. B relies on:
- the post-A `RevokeLaunchSession` (`active` or `consumed`, returning `prior_status`);
- `consumed → revoked` existing, which the §3.4 replay-after-revoke case needs;
- A's inverted characterization test (`launch_two_phase_integration_test.go:703-795`) staying green.

## 7. MOCK over HTTP; no in-process shortcut

- `MockCasinoProvider` gains a **bootstrap client**. It signs requests with the same per-tenant
  derived key as `CallbackPayload`/`SignRawBody`, and tests send them **over HTTP** to the real
  route on an `httptest` server.
- Nothing calls `BootstrapLaunch` in-process as a stand-in for the vendor. Its unit tests use a
  real `WithTenant` transaction.
- The never-consumed TTL rule and the synthetic-adapter outbound tripwire are unchanged.
- **Label: MOCK.** A real vendor is PROVIDER DEPENDENT, and needs CAS-PLAYER-REF-1 closed first.

## 8. Invariants

| ID | Invariant |
|---|---|
| BS-0 | Redeem+Recheck is the first statement of the transaction, and the body is parsed only from verified bytes. |
| BS-1 | The binding predicates, including `provider_game_id`, are inside the consuming UPDATE. |
| BS-2 | A step 1–3 refusal writes nothing. A step 4 **definitive** denial only revokes and audits. An evaluation error writes nothing and returns 5xx. |
| BS-3 | At most one consume per session, and at most one bootstrap row per session. |
| BS-4 | A replay returns the stored response only when `token_hash` and `request_digest` both match and the session is `consumed`. |
| BS-5 | The tenant and provider come from the route and the credential only. |
| BS-6 | The raw token and its hash never appear in logs, audit rows, errors or traces. |
| BS-7 | `player_ref` is opaque, random and provider-scoped. |
| BS-8 | Every step 1–3 refusal and every non-matching replay gets one byte-identical response. The 403 body is constant. |
| BS-9 | The new tables' RLS excludes player, platform-admin, platform-service and `app.acting_*` sessions. |

## 9. Tests (plan §5-B; QA W2 "B"; T-1/T-2)

**ADV.** Each of these returns the uniform 401 **with the session unchanged and no rows written**:
- an unsigned or wrongly signed request;
- an unknown slug;
- cross-tenant;
- cross-provider;
- a mismatch of mode, asset or **provider game**;
- an expired token (fixture timestamps, no sleep);
- an already-consumed token with a new `request_id`;
- a revoked token;
- **replay after A's revoke**;
- **a reused `request_id` with a different token hash**;
- **a reused `request_id` with the same token but different fields** (digest mismatch, SB-6);
- a body carrying `tenant_id` (an unknown field);
- **SB-1:** a handle revoked between phase-1 verification and the transaction.

**Refusal-does-not-consume.** Every step 1–3 case, then the legitimate request → success.

**Gate denial (Q6):**
- game disabled, capability disabled, or RG-excluded → the constant 403; the session is `revoked`;
  the audit row carries `prior_status=active` and the reason enum; no bootstrap row; and a later
  `postBet` on that session is refused;
- an **injected evaluation error** → 5xx, session `active`, nothing written;
- a revoke returning `false` (forced) → rollback and 5xx.

**CON (`-race -count=50`):**
- two concurrent consumes with different `request_id`s → **exactly one** 200, one bootstrap row,
  `consumed` once;
- the same `request_id` → two identical 200s and one row.

**IDM:** replay → a byte-identical response and no new audit row.

**AU / secrecy (C-103-5):** one `casino.launch_bootstrapped` row per success. A scan of every audit
row, captured log, error string and trace attribute produced by **every new success and error
path** finds neither the raw token nor its hash.

**TI:**
- A's route never resolves B's token;
- `player_ref` differs across providers and across tenants.

**RLS (SB-7):** for both new tables, sessions with player, platform-admin, platform-service or
`app.acting_*` GUCs see and write nothing.

**MIG:**
- the insert trigger refuses a row whose hash, provider or status does not match its session;
- the `request_digest` format CHECK;
- UPDATE, DELETE and TRUNCATE are refused;
- `down` refuses while rows exist.

**Lock order:** the bootstrap is added to the harness.

**MUT (must be killed):**
- drop `provider_id` from the CAS;
- **drop `provider_game_id` from the CAS (SB-2)**;
- drop `expires_at > now()`;
- drop `status='active'`;
- check the provider after the CAS;
- **move Redeem+Recheck after the session lookup (SB-1)**;
- replay without the hash comparison;
- **replay without the digest comparison (SB-6)**;
- replay without the `consumed` check;
- a lazy `expired` write on refusal;
- revoke on an evaluation error;
- drop `UNIQUE (launch_session_id)`.

## 10. Alternatives rejected

| Alternative | Why rejected |
|---|---|
| Reuse `ResolveLaunchToken` | S-5 |
| Leave the session `active` on a gate denial | §3.3 |
| Revoke on a binding mismatch | DoS, oracle |
| Revoke on an evaluation error | Q6: a transient fault is not a decision |
| Session lookup before Redeem+Recheck | SB-1: a revoked credential would still get a read |
| A digest including the token | Duplicates `token_hash`, and adds token-derived material |
| HMAC or UUIDv5 player refs | A new secret, or reversible |
| Storing idempotency data on the session row | A owns that row's trigger this round |
| Player-authenticated bootstrap | ADR 0025 §3 |

## 11. Open items

1. **CAS-PLAYER-REF-1** (registered): must close before any real vendor (SB-8).
2. **CAS-BET-REQUIRES-BOOTSTRAP-1** (registered, SB-5): whether a real-money bet should require a
   `consumed` session.
3. **CAS-GAME-KILL-BET-1** (registered, SB-4): `postBet` never reads `casino_games.status`. It blocks
   real-money casino launch, not B.
4. **Q4 (ACCEPTED; rationale corrected, SB-3).** Bootstrap does not re-run risk or jurisdiction.
   `postBet` re-checks risk (with the frozen jurisdiction), capability, RG and the provider binding
   on every bet. It does **not** re-run jurisdiction resolution, the game blocklist or
   `casino_games.status`. The last of these is CAS-GAME-KILL-BET-1, and bootstrap's own step 4 does
   check `casino_games.status`.
5. **Deleting `ResolveLaunchToken`** after B lands: casino's call.

## 12. Review disposition (revision 2)

| Finding | Resolution |
|---|---|
| SB-1 / C-103-1 | §3.2 step 2a; BS-0; SB-1 test; mutant |
| SB-2 / C-103-2 | §3.2 step 5; BS-1; mutant |
| SB-3 / C-103-6 | §1 and §11 item 4 corrected; the `:311` citation changed to `:314` |
| SB-4 | CAS-GAME-KILL-BET-1 referenced (§1, §11 item 3) |
| SB-5 | CAS-BET-REQUIRES-BOOTSTRAP-1 referenced (§11 item 2) |
| SB-6 / C-103-4 | §3.4 `request_digest`; §5 column; tests; mutant |
| SB-7 / C-103-5 | §5 exclusion list; BS-9; RLS test |
| SB-8 | §3.5; §11 item 1 |
| C-103-3 (Q6) | §3.3 conditions; tests |
| C-103-5 (raw-token scan) | §9 AU/secrecy |
| C-103-6 (registry rows) | Existing ids referenced; no new id invented |
| Q4 | §11 item 4 |
| Q5 | §3.6 |
| Q6 | §3.3 |
| Product-owner-proxy | ACCEPT; no change |

**Handover/DoD:**
- **Artefacts:**
  - this ADR ACCEPTED;
  - A merged;
  - B merged with the §5 migration (number allocated by the orchestrator), `casino/bootstrap.go`,
    the route, the MOCK bootstrap client and the §9 tests;
  - a `docs/integrations/` contract page;
  - HANDOVER row: "casino bootstrap: MOCK over HTTP; real vendor PROVIDER DEPENDENT (after
    CAS-PLAYER-REF-1)".
- **Registry (orchestrator):** CAS-PLAY-BOOTSTRAP-1 → IMPLEMENTED (MOCK).

## 13. Implementation status (2026-09-28, workstream B, branch `prh2-b-cas-play-bootstrap`)

**Status: MOCK. Every line item of §3 implemented as specified**, on top of A (migration 0108,
merged). Migration number **0115** (placeholder - the orchestrator renumbers at merge, §5/Rule 3;
0109-0114 belong to other lanes not yet merged onto this branch).

- `internal/casino/bootstrap.go`: `Orchestrator.BootstrapLaunch`, the exact §3.2 step order
  (Redeem+Recheck first via the existing `redeemVerified`, unmodified; the session lookup FOR
  NO KEY UPDATE (originally FOR UPDATE - see the §4 amendment and the "Lock order" item below); the
  idempotency lookup; the binding check; the two Q4 gates plus RG; the binding-aware
  CAS; the player-ref upsert; the idempotency insert via `db.IdempotentInsert` (the SAVEPOINT
  primitive already used by `ledger.Post`/`withdrawal.RequestWithdrawal`/`payments.
  InitiateDeposit`) - chosen specifically because it lets the required-once-per-request `Redeem`
  (single-use by design, `VerifiedCallback.Redeem`) stay the transaction's genuine first statement
  even on the rare cross-token same-request_id race, rather than needing a second `VerifyCallback`
  call the token could not survive; audit, then return).
- `ResolveLaunchToken` is **not reused, and not deleted** in this change (§11 item 5: "casino's
  call" - it still has test-only callers this change does not touch; a follow-up may remove it).
- `internal/casino/mock.go`: `MockCasinoProvider.BootstrapPayload` (§7's "bootstrap client") -
  signs with the same per-tenant derived key as `CallbackPayload`/`SignRawBody`. Every test sends
  it over a real `net/http` round trip to the real route on an `httptest` server; nothing calls
  `BootstrapLaunch` as a vendor stand-in.
- `internal/httpserver/casino_bootstrap_handlers.go` + the new line in `casino_routes.go`:
  `POST /v1/webhooks/casino/{tenantSlug}/{providerID}/launch-bootstrap`, sharing the existing
  preamble/admission bulkhead/credential surface with the bet/win/rollback callback route (same
  domain tag, same scheme - a distinct path, never a distinct domain).
- **Byte-identical replay (BS-4), corrected during implementation:** PostgreSQL's JSONB storage
  does not preserve the original key order/whitespace of an inserted value - `casino_launch_
  bootstraps.response` is stored for audit/debugging visibility, but a replay's own response is
  re-marshaled from the session's own denormalized columns (the same Go struct, same field order,
  same `json.Marshal` call), which is what actually guarantees byte-identical output. Found by the
  author's own `TestBootstrapLaunch_Replay_ByteIdenticalNoNewAudit` before this ADR's own review.
- **Down-migration RLS blindness, corrected during implementation:** both new tables carry FORCE
  ROW LEVEL SECURITY with no policy admitting a migration-context connection (no `app.tenant_id` is
  ever set while migrations run), so a plain `SELECT ... FROM casino_launch_bootstraps` "refuse
  while rows exist" guard would see zero rows regardless of how many exist - the exact migration
  0048/0092/0107 lesson already recorded in this codebase. Fixed with `ADD CONSTRAINT ... CHECK
  (false)`, which validates every existing row at the storage level unconditionally, regardless of
  RLS (the same "the check IS the check" principle as 0107's own unique-index-build guard). Found
  by the author's own `TestMigration0115_DownRefusesWhileRowsExist`.
- **Two MUT-list mutants are structurally unreachable, not merely untested** ("a lazy `expired`
  write on refusal"; "revoke on an evaluation error"): `BootstrapLaunch` runs the entire contract
  in ONE transaction whose commit/rollback is driven solely by whether the closure returns nil: a
  refusal or an evaluation error always returns non-nil, so `pool.WithTenant` always rolls back the
  WHOLE transaction - any write attempted on that path (a lazy `expired` write; a revoke) can never
  survive to be observed, regardless of whether the write is present in the code. Verified
  empirically, not merely reasoned about: both were physically added, and the existing tests
  (`TestBootstrapLaunch_ADV_Expired`; `TestBootstrapLaunch_EvaluationError_RollsBackNeverDenies`)
  still passed with them in place (the write vanished with the rollback) - see the evidence file.
  This is a stronger guarantee than the MUT item anticipated, not a gap.
- **Interaction with the pre-existing B2C "play simulation" routes, reported rather than silently
  changed:** `casino_play_handlers.go`'s `requireActiveUnexpiredSession` (documented there as
  security review finding P2-2) accepts only `'active'`, never `'consumed'` - a session a real
  bootstrap has consumed is not usable through `POST /v1/me/casino/sessions/{id}/wager` (or its
  win/rollback siblings) at all, confirmed empirically. This ADR names only `postBet`'s own status
  allow-list (already `active`/`consumed`, CAS-SESSION-EXPIRY-1); it never names
  `casino_play_handlers.go`. Widening a named, already-reviewed security gate is outside this
  workstream's own contract and is not done here - see the orchestrator report for this decision.
  The REAL provider path (a genuinely-signed callback through `postBet`) is unaffected and is what
  `TestCasinoBootstrap_RealProviderBetWorksAfterBootstrap` (`internal/httpserver`) exercises end to
  end over HTTP.
- **Lock order (§4) - CORRECTED (2026-09-28, architect review F-1):** the analysis this item
  originally recorded was wrong, and is kept below (struck through in spirit, not in fact - the
  full original text is preserved so the mistake and its correction are both on the record) rather
  than silently deleted:

  > ~~`BootstrapLaunch`'s own order - Redeem+Recheck (no lock), then the session row `FOR UPDATE`,
  > then RG's person-scoped `pg_advisory_xact_lock` (step 4) - was checked against every other
  > resource-acquisition order in this package and matches the ADR's own specified order exactly;
  > no other code path acquires RG's advisory lock and then a `casino_launch_sessions` row lock
  > (the only shape that could form a cycle with this one), so no synthetic two-resource ABBA
  > reproduction was constructed - doing so would have simulated a code path that does not exist.
  > The CON tests (...) found no deadlock empirically. A formal harness entry
  > (`lockorder_harness_test.go`) is NOT added in this change; flagged as a scope decision for the
  > orchestrator, not silently skipped.~~

  This missed an **implicit** lock acquisition: `postBet`'s `BindProviderRound` call inserts into
  `casino_provider_rounds`, whose foreign key to `casino_launch_sessions` takes a `FOR KEY SHARE`
  lock on the session row with no explicit SQL anywhere naming it. `postBet` therefore DOES acquire
  RG's advisory lock and then (via that FK) a `casino_launch_sessions` row lock - exactly the shape
  the original analysis said did not exist - and bootstrap's own session-row-then-RG order is the
  same two resources in the opposite sequence: a genuine ABBA deadlock (`40P01`). The architect
  review caught this from first principles (reading `orchestrator.go`'s lock sequence against
  `bootstrap.go`'s), not from a failing test - the CON tests never triggered it, because they never
  race a bootstrap against a bet on the same session, only against another bootstrap. See §4's own
  amendment for the fix (`FOR NO KEY UPDATE`) and the now-added formal harness entry,
  `TestLockOrder_BootstrapAndFirstBetOfRound_NoDeadlock`
  (`internal/casino/lockorder_integration_test.go`), which this ADR no longer flags as a deferred
  scope decision - it is done.
- **Tests:** `internal/casino/bootstrap_integration_test.go`, `bootstrap_rls_integration_test.go`,
  `bootstrap_sb1_integration_test.go`, `migration_0115_bootstrap_integration_test.go`;
  `internal/httpserver/casino_bootstrap_integration_test.go`. Mutation evidence:
  `docs/plans/payment-readiness/evidence/prh2-casino-b-mutation-kill.txt`.
- **Registry:** proposed text reported to the orchestrator, not written directly to
  `docs/governance/task-registry.md` per this workstream's own instruction.
