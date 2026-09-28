# Architect / QA / POP review — PRH-2 B (CAS-PLAY-BOOTSTRAP-1), 2026-09-28

**Reviewer:** `architect`, also covering the QA and product-owner-proxy lenses. The orchestrator recorded this review.

**Scope:** `e720001`, based on `a7b2720`. Read-only.

## Verdict: ACCEPT WITH CONDITIONS

- **F-1 blocks the merge.**
- F-2 to F-10 must be done before the registry row closes.
- F-11 and F-12 are registry and orchestrator actions.

**The design conforms to ADR 0103 §3.2, step for step:**
- Redeem+Recheck is first.
- Only the verified body is used.
- The session is locked, then the idempotency row is looked up.
- The binding is checked.
- The gates run in order: game, capability, RG.
- A denial revokes, audits and commits. An evaluation error rolls back.
- The CAS carries all seven predicates.
- Then the player ref, the idempotency insert and the audit. There is no token or hash in the audit.

The provider abstraction is clean. The IO-1C guard covers the bootstrap closure.

**The author's key resolutions:**
- (a) Retrying on a unique violation inside the `IdempotentInsert` SAVEPOINT: **sound**. It is equivalent to the ADR's approach and avoids a second Redeem.
- (b) Replay re-marshalled from immutable columns: **sound**. The stored `response` becomes an audit copy.
- (c) The `ADD CONSTRAINT … CHECK (false)` down-guard under FORCE RLS: **sound**. Record it as the codebase pattern.
- (d) `ResolveLaunchToken` kept: acceptable, but it must be marked DO-NOT-USE.

**Open question (P2-2):**
- **No regression.** Nothing auto-bootstraps in MOCK mode: there are no non-test callers of `BootstrapPayload` or `BootstrapLaunch`. MOCK B2C sessions stay `active`, and the play routes behave as before.
- **Recommendation:** keep `requireActiveUnexpiredSession` `active`-only. A consumed session belongs to the vendor channel and has no in-play bound yet.
  - Keep the ~2-minute MOCK-play window as a residual.
  - The long-term fix is to turn the simulation routes into a MOCK *vendor* client (launch, then bootstrap, then signed callbacks), decided together with CAS-BET-REQUIRES-BOOTSTRAP-1.
  - Security rules on this.

## Findings

| ID | Sev | Lens | Finding | Required change |
|---|---|---|---|---|
| F-1 | MEDIUM (**merge-blocking**, an ADR §4 condition) | Arch | **The lock-order claim is refuted.** Bootstrap takes the session `FOR UPDATE`, then the RG person lock (`rg.go:607`). `postBet` on an `active` session takes the RG lock (`orchestrator.go:1570`), then `BindProviderRound` (`:1785`) inserts into `casino_provider_rounds`. That table's FK to `casino_launch_sessions` (0080:58) takes `FOR KEY SHARE` on the same row, which conflicts with `FOR UPDATE`. This is an ABBA deadlock (40P01) between the first bet of a round and a bootstrap. It is a liveness problem only; there is no ledger effect. | Change the lock to `SELECT … FOR NO KEY UPDATE`: it still serialises bootstraps and A's revoke, and it is compatible with FK KEY SHARE. Add a real `lockorder_harness_test.go` interleaving of bootstrap against the first `postBet` of a round, which deadlocks under `FOR UPDATE` (the mutant) and passes under the fix. Amend ADR §4 and §13. Security acknowledges per §4. |
| F-2 | MEDIUM | QA | The HTTP mapping is untested: no test covers a step-3 refusal, a replay mismatch, a wrong signature (the uniform 401 body, BS-8), or the constant 403. | Add httpserver tests. |
| F-3 | MEDIUM | QA | Missing tests: the refusal-does-not-consume sequence (each step 1–3 failure, then the legitimate request → 200); mode, asset and game mismatches via `BootstrapLaunch`; an already-consumed token with a new `request_id` (sequential); an unknown slug; a gate denial then a refused `postBet`; a forced revoke returning false → `ErrBootstrapInvariantBroken` → 5xx; `player_ref` differing across tenants; RLS write refusal for the four non-player GUCs. | Add them. |
| F-4 | LOW-MED | QA | Evidence mutants 10 and 11 were applied in their weak, equivalent form. | Re-run 10′ (the refusal commits an `expired` write) and 11′ (an evaluation error is classified as a denial). They should be killed by the existing tests. Restate the result as 12/12 with an equivalent-mutant note. |
| F-5 | LOW-MED | Arch/Sec | The ADR §5 grants in `deploy/init-app-role.sql` are missing. | Add REVOKE ALL / GRANT SELECT, INSERT for both tables, following the `casino_callback_rejections` pattern. |
| F-6 | MEDIUM | Sec/QA | The C-103-5 secrecy scan covers only the success audit. | Capture logs on every new path, including denial audits, error strings and 5xx, and scan them for the raw token and its hash. |
| F-7 | LOW | Arch | `IdempotentInsert`'s `constraintName` is discarded. | Branch on `casino_launch_bootstraps_once_per_request`; treat any other unique violation as an invariant 5xx. |
| F-8 | LOW | Arch | The ADR text (§3.4, BS-4, step 6, §5) and code comments are stale on replay, retry and the down-guard. | Amend them. |
| F-9 | LOW | Arch/POP | The `ResolveLaunchToken` comment invites reuse. | Reword it as DO-NOT-USE (S-5), and register a deletion follow-up. |
| F-10 | LOW | QA | The `-race` count is inconsistent (50 vs "20+"). | Record the exact command and output in the evidence file. |
| F-11 | INFO | POP | B does not fix the MOCK B2C ~2-minute play window that motivated CAS-PLAY-BOOTSTRAP-1. | The orchestrator records the endpoint as IMPLEMENTED (MOCK) and keeps the MOCK-play limitation open. Add the HANDOVER row. |
| F-12 | INFO | Arch | A real aggregator will need an adapter-side parse hook for the bootstrap body. | Record it as deferred in the contract page and §13. |
