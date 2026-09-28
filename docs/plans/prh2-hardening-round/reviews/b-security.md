# Security review — PRH-2 B (CAS-PLAY-BOOTSTRAP-1), 2026-09-28

**Reviewer:** `security` (the hard gate under ADR 0103). The orchestrator recorded this review.

**Scope:** `e720001`, based on `a7b2720`. Private DB, dropped afterwards.

**Note:** this review predates the architect's F-1 deadlock finding (`b-architect-qa-pop.md`). The lock-mode fix (`FOR NO KEY UPDATE`) needs security's acknowledgement under ADR 0103 §4 on the fixed head.

## Verdict: ACCEPT WITH CONDITIONS

The S-5 order and every ADR 0103 security condition are implemented and tested.

## Verified

**Transaction order**
- **Redeem+Recheck is the first statement** (SB-1, C-103-1), proved by statement capture.
- The body is decoded only from the verified bytes, with `DisallowUnknownFields`.
- The session is locked by `(token_hash, tenant_id)`.

**Refusals**
- **Uniform refusal:** every step 1–3 failure and every non-matching replay returns the same 401 `"callback rejected"`. The reason appears only in the server log, and nothing is written.
- **Gate denial:** only `game_inactive`, `capability_denied` and `rg_ineligible` count as definitive.
  - A definitive denial revokes, asserts `prior == active && revoked`, audits with `prior_status` and the reason, and commits.
  - The response is a constant 403.
  - An evaluation error rolls back and returns 5xx.

**Consume and idempotency**
- **Binding-aware CAS** (SB-2), with all predicates.
- **Player ref:** insert-or-select, append-only, random and provider-scoped.
- **The idempotency savepoint retry is sound:**
  - a unique violation surfaces only after the competitor commits;
  - a mismatch rolls back the whole transaction;
  - a same-token race is serialised by the row lock;
  - because `Redeem` is single-use, retrying inside the transaction is the only correct shape.
- **Replay** is rebuilt from the session columns plus the stored `player_ref`, and writes nothing.

**Secrecy (BS-6):** there is no token or hash in the audit. No constraint, trigger or FK message carries the hash, and pgx does not echo bind parameters.

**RLS:** both tables have the full exclusion set. The test seeds a visible row first, so it is not vacuous.

**Down guard:** `CHECK (false)` validates the heap regardless of RLS, so it is sound.

**Unkilled mutants 10 and 11: the argument holds.** `WithTenant` commits only when the closure returns nil, which happens on exactly three paths: success, replay (which writes nothing) and definitive denial. The definitive denial is the only commit-on-refusal path. The ADV "session unchanged" tests guard future refactors.

| Mutant re-killed | Result | Killed by |
|---|---|---|
| CAS `expires_at > now()` removed | KILLED | `TestCasConsumeSessionForBootstrap_RefusesNonActiveAndExpired/already_expired` |
| Provider binding replaced with a no-op | KILLED | `TestCasConsumeSessionForBootstrap_BindingPredicate/wrong_provider_id` |

**Tests (local, not CI):**
- `-race -tags integration` casino: ok;
- httpserver `Bootstrap|CasinoPlay|Casino.*Callback`: 19 PASS.

## Findings

| ID | Sev | Finding | Required change |
|---|---|---|---|
| B-C1 | Low-Med (before merge) | `deploy/init-app-role.sql` was not updated. Its broad grant at `:85` re-grants UPDATE and DELETE on re-run. This is the same issue as architect F-5. | Add an idempotent block after the broad grant: `REVOKE ALL`, then `GRANT SELECT, INSERT` on `casino_launch_bootstraps` and `casino_provider_player_refs`. |
| B-C2 | Low | 0115 adds `UNIQUE (id, tenant_id)` on `casino_launch_sessions`, duplicating 0080's `casino_launch_sessions_id_tenant_key`. That contradicts ADR 0103 §5, "no change to `casino_launch_sessions`". | Drop the ALTER from up and down. The FK can reference the 0080 constraint. |
| B-I1 | Info | The step-3 provider mismatch is killed only through the refusal-reason assertion; the CAS also refuses independently. | None. |
| B-I2 | Info | The CAS uses DB `now()` and step 3 uses Go `time.Now()`. Skew can only cause a uniform refusal. | None. |

**Carried registry items (not B blockers):**
- CAS-PLAYER-REF-1, before any real vendor;
- CAS-GAME-KILL-BET-1;
- CAS-BET-REQUIRES-BOOTSTRAP-1.

## Ruling on P2-2 (`requireActiveUnexpiredSession`)

**Leave it `active`-only. Do not widen it to `consumed`.**
- The simulation routes are player-driven stand-ins for vendor calls, reachable only outside production.
- A `consumed` session is owned by the vendor's signed webhook path. Widening would create a second, unsigned driver for the same round's money, and would blur the boundary P2-2 protects.

**No regression:** nothing in MOCK mode auto-bootstraps.
- `BootstrapPayload` has no non-test caller.
- `BootstrapLaunch`'s only caller is the vendor route.

**Recommended pin (Low):** a test that a `consumed` session is refused by the simulation wager route, with the behaviour documented in the contract page.
